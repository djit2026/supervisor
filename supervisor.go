package supervisor

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type Strategy int

const (
	OneForOne Strategy = iota
	OneForAll
	RestForOne
)

type Supervisor struct {
	mu          sync.Mutex
	workers     []*workerRuntime
	wg          sync.WaitGroup
	started     bool
	events      chan Event
	eventBuffer int
	ctx         context.Context
	cancel      context.CancelFunc
	strategy    Strategy
	restartCh   chan struct{}
	readyCh     chan struct{}
	restartHold bool
	barrier     *restartBarrier
}

type restartBarrier struct {
	pending      map[*workerRuntime]struct{}
	restartGates map[*workerRuntime]chan struct{}
	startGates   map[*workerRuntime]chan struct{}
	execGates    map[*workerRuntime]chan struct{}
	restarted    map[*workerRuntime]struct{}
	started      map[*workerRuntime]struct{}
	skipped      map[*workerRuntime]struct{}
	order        []*workerRuntime
	nextRestart  int
	nextStart    int
	ordered      bool
}

func NewSupervisor(opts ...Option) *Supervisor {
	readyCh := make(chan struct{})
	close(readyCh)

	s := &Supervisor{
		eventBuffer: 100,
		restartCh:   readyCh,
		readyCh:     readyCh,
	}

	for _, opt := range opts {
		opt(s)
	}

	s.events = make(chan Event, s.eventBuffer)

	return s
}

func (s *Supervisor) Add(spec WorkerSpec) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		panic("cannot add workers after Start")
	}

	rt := &workerRuntime{
		spec: spec,
		state: WorkerState{
			Name:   spec.Name,
			Status: StatusStarting,
		},
	}

	s.workers = append(s.workers, rt)
}

func (s *Supervisor) Start(parent context.Context) error {
	s.mu.Lock()

	if s.started {
		s.mu.Unlock()
		return fmt.Errorf("already started")
	}

	// supervisor context
	s.ctx, s.cancel = context.WithCancel(parent)
	s.started = true

	workers := make([]*workerRuntime, len(s.workers))
	copy(workers, s.workers)

	s.mu.Unlock()

	for _, w := range workers {
		s.wg.Add(1)
		go s.runWorker(w)
	}

	s.wg.Wait()

	close(s.events)

	return nil
}

func (s *Supervisor) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.started {
		return fmt.Errorf("not started")
	}

	if s.cancel != nil {
		s.cancel()
	}

	return nil
}

func (s *Supervisor) Subscribe() <-chan Event {
	return s.events
}

func (s *Supervisor) Metrics() []WorkerSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []WorkerSnapshot

	for _, w := range s.workers {
		out = append(out, WorkerSnapshot{
			Name:     w.spec.Name,
			Restarts: atomic.LoadInt64(&w.metrics.Restarts),
			Failures: atomic.LoadInt64(&w.metrics.Failures),
			Success:  atomic.LoadInt64(&w.metrics.Success),
			LastLatency: time.Duration(
				atomic.LoadInt64(&w.metrics.LastLatency),
			),
		})
	}

	return out
}

func (s *Supervisor) runWorker(w *workerRuntime) {
	defer s.wg.Done()

	pendingRestart := false
	var pendingRestartInstanceID int64

	for {
		ch := s.restartGate(w)
		select {
		case <-ch:
		case <-s.ctx.Done():
			w.setStatus(StatusStopped)

			s.emit(Event{
				Worker: w.spec.Name,
				Type:   EventStopped,
				Time:   time.Now(),
			})
			return
		}

		if s.ctx.Err() != nil {
			w.setStatus(StatusStopped)

			s.emit(Event{
				Worker: w.spec.Name,
				Type:   EventStopped,
				Time:   time.Now(),
			})
			return
		}

		if pendingRestart {
			w.incrementRestart()
			w.metricsRestart()

			s.emit(Event{
				Worker:     w.spec.Name,
				InstanceID: pendingRestartInstanceID,
				Type:       EventRestarted,
				Time:       time.Now(),
				Restarts:   w.getRestarts(),
			})

			startCh := s.workerRestartedForRestart(w)
			select {
			case <-startCh:
			case <-s.ctx.Done():
				w.setStatus(StatusStopped)

				s.emit(Event{
					Worker: w.spec.Name,
					Type:   EventStopped,
					Time:   time.Now(),
				})
				return
			}

			pendingRestart = false
		}

		ctx := w.newContext(s.ctx)

		instanceID := w.nextInstance()

		w.setStatus(StatusRunning)

		s.emit(Event{
			Worker:     w.spec.Name,
			InstanceID: instanceID,
			Type:       EventStarted,
			Time:       time.Now(),
			Restarts:   w.getRestarts(),
		})

		execCh := s.workerStartedForRestart(w)
		select {
		case <-execCh:
		case <-s.ctx.Done():
			w.setStatus(StatusStopped)

			s.emit(Event{
				Worker:     w.spec.Name,
				InstanceID: instanceID,
				Type:       EventStopped,
				Time:       time.Now(),
			})
			return
		}

		start := time.Now()
		err := s.safeRun(ctx, w)
		wasCancelled := ctx.Err() != nil
		duration := time.Since(start)

		// heartbeat check overrides err
		if !wasCancelled && w.isStuck(w.spec.HeartbeatTTL) {
			err = fmt.Errorf("worker stuck: no heartbeat")
		}

		// update execution state
		w.updateExecution(err, duration)

		// metrics
		if err != nil && !wasCancelled {
			w.metricsFail()
		} else {
			w.metricsSuccess()
		}
		w.metricsLatency(duration)

		// emit failure
		if err != nil && !wasCancelled {
			s.emit(Event{
				Worker:     w.spec.Name,
				InstanceID: instanceID,
				Type:       EventFailed,
				Err:        err,
				Time:       time.Now(),
				Restarts:   w.getRestarts(),
				Latency:    duration,
			})

			// apply strategy immediately on failure
			s.applyStrategy(w)
		}

		if wasCancelled {
			if s.ctx.Err() != nil {
				w.setStatus(StatusStopped)

				s.emit(Event{
					Worker:     w.spec.Name,
					InstanceID: instanceID,
					Type:       EventStopped,
					Time:       time.Now(),
				})
				return
			}

			w.setStatus(StatusStopped)

			s.emit(Event{
				Worker:     w.spec.Name,
				InstanceID: instanceID,
				Type:       EventStopped,
				Time:       time.Now(),
			})

			s.workerStoppedForRestart(w)
			pendingRestart = true
			pendingRestartInstanceID = instanceID
			continue
		}

		s.workerStoppedForRestart(w)

		// restart decision
		if !s.shouldRestart(w, err) {
			w.setStatus(StatusStopped)

			s.emit(Event{
				Worker:     w.spec.Name,
				InstanceID: instanceID,
				Type:       EventStopped,
				Time:       time.Now(),
			})
			s.workerSkippedRestart(w)
			return
		}

		// throttling
		if !w.allowRestart() {
			w.setStatus(StatusStopped)

			s.emit(Event{
				Worker:     w.spec.Name,
				InstanceID: instanceID,
				Type:       EventThrottled,
				Err:        fmt.Errorf("restart throttled"),
				Time:       time.Now(),
			})
			s.workerSkippedRestart(w)
			return
		}

		pendingRestart = true
		pendingRestartInstanceID = instanceID

		// backoff
		if w.spec.Backoff != nil {
			delay := w.spec.Backoff.Next(w.getRestarts())

			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
				timer.Stop()

			case <-ctx.Done():
				timer.Stop()

				// check if supervisor is shutting down
				if s.ctx.Err() != nil {
					w.setStatus(StatusStopped)

					s.emit(Event{
						Worker:     w.spec.Name,
						InstanceID: instanceID,
						Type:       EventStopped,
						Time:       time.Now(),
					})
					return
				}

				w.setStatus(StatusStopped)
				s.workerStoppedForRestart(w)
				continue
			}
		}
	}
}

func (s *Supervisor) stopWorker(w *workerRuntime) {
	w.cancelInstance()
}

func (s *Supervisor) safeRun(ctx context.Context, w *workerRuntime) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()

	return w.spec.Run(ctx, w.heartbeat)
}

func (s *Supervisor) emit(e Event) {
	if s.events == nil {
		return
	}

	select {
	case s.events <- e:
	default:
		// drop event if full
	}
}

func (s *Supervisor) restartGate(w *workerRuntime) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.restartHold && s.barrier != nil {
		if ch, ok := s.barrier.restartGates[w]; ok {
			return ch
		}
		return s.readyCh
	}

	return s.restartCh
}

func (s *Supervisor) applyStrategy(failed *workerRuntime) {
	s.mu.Lock()

	var toStop []*workerRuntime

	switch s.strategy {

	case OneForOne:
		s.mu.Unlock()
		return

	case OneForAll:
		for _, w := range s.workers {
			toStop = append(toStop, w)
		}
		s.holdRestartsLocked(toStop, false)

	case RestForOne:
		found := false
		for _, w := range s.workers {
			if w == failed {
				found = true
			}
			if found {
				toStop = append(toStop, w)
			}
		}
		s.holdRestartsLocked(toStop, true)
	}

	s.mu.Unlock()

	for _, w := range toStop {
		s.stopWorker(w)
	}
}

func (s *Supervisor) holdRestartsLocked(workers []*workerRuntime, ordered bool) {
	if !s.restartHold {
		s.restartCh = make(chan struct{})
		s.restartHold = true
		s.barrier = &restartBarrier{
			pending:      make(map[*workerRuntime]struct{}, len(workers)),
			restartGates: make(map[*workerRuntime]chan struct{}, len(workers)),
			startGates:   make(map[*workerRuntime]chan struct{}, len(workers)),
			execGates:    make(map[*workerRuntime]chan struct{}, len(workers)),
			restarted:    make(map[*workerRuntime]struct{}, len(workers)),
			started:      make(map[*workerRuntime]struct{}, len(workers)),
			skipped:      make(map[*workerRuntime]struct{}, len(workers)),
			ordered:      ordered,
		}
	} else if ordered {
		s.barrier.ordered = true
	}

	for _, w := range workers {
		if w.isStopped() {
			continue
		}
		if _, ok := s.barrier.restartGates[w]; !ok {
			s.barrier.restartGates[w] = make(chan struct{})
			s.barrier.startGates[w] = make(chan struct{})
			s.barrier.execGates[w] = make(chan struct{})
			s.barrier.order = append(s.barrier.order, w)
		}
		s.barrier.pending[w] = struct{}{}
	}

	if len(s.barrier.pending) == 0 {
		s.releaseRestartPhaseLocked()
	}
}

func (s *Supervisor) workerStoppedForRestart(w *workerRuntime) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.restartHold || s.barrier == nil {
		return
	}

	if _, ok := s.barrier.pending[w]; !ok {
		return
	}

	delete(s.barrier.pending, w)
	if len(s.barrier.pending) > 0 {
		return
	}

	s.releaseRestartPhaseLocked()
}

func (s *Supervisor) workerRestartedForRestart(w *workerRuntime) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.restartHold || s.barrier == nil {
		return s.readyCh
	}

	ch, ok := s.barrier.startGates[w]
	if !ok {
		return s.readyCh
	}

	s.barrier.restarted[w] = struct{}{}
	if s.barrier.ordered {
		s.releaseRestartPhaseLocked()
	}
	if s.restartPhaseCompleteLocked() {
		s.releaseStartPhaseLocked()
	}

	return ch
}

func (s *Supervisor) workerStartedForRestart(w *workerRuntime) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.restartHold || s.barrier == nil {
		return s.readyCh
	}

	ch, ok := s.barrier.execGates[w]
	if !ok {
		return s.readyCh
	}

	s.barrier.started[w] = struct{}{}
	if s.barrier.ordered {
		s.releaseStartPhaseLocked()
	}
	if s.startPhaseCompleteLocked() {
		s.releaseExecPhaseLocked()
	}

	return ch
}

func (s *Supervisor) workerSkippedRestart(w *workerRuntime) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.restartHold || s.barrier == nil {
		return
	}

	s.barrier.skipped[w] = struct{}{}
	if s.barrier.ordered {
		s.releaseRestartPhaseLocked()
		s.releaseStartPhaseLocked()
	}
	if s.restartPhaseCompleteLocked() {
		s.releaseStartPhaseLocked()
	}
	if s.startPhaseCompleteLocked() {
		s.releaseExecPhaseLocked()
	}
}

func (s *Supervisor) restartPhaseCompleteLocked() bool {
	if !s.restartHold || s.barrier == nil {
		return false
	}

	return len(s.barrier.restarted)+len(s.barrier.skipped) >= len(s.barrier.order)
}

func (s *Supervisor) startPhaseCompleteLocked() bool {
	if !s.restartHold || s.barrier == nil {
		return false
	}

	return len(s.barrier.started)+len(s.barrier.skipped) >= len(s.barrier.order)
}

func (s *Supervisor) releaseRestartPhaseLocked() {
	if !s.restartHold || s.barrier == nil {
		return
	}

	if !s.barrier.ordered {
		for _, ch := range s.barrier.restartGates {
			close(ch)
		}
		s.barrier.restartGates = nil
		return
	}

	for s.barrier.nextRestart < len(s.barrier.order) {
		w := s.barrier.order[s.barrier.nextRestart]
		s.barrier.nextRestart++

		if _, skipped := s.barrier.skipped[w]; skipped {
			continue
		}

		ch, ok := s.barrier.restartGates[w]
		if !ok {
			continue
		}

		delete(s.barrier.restartGates, w)
		close(ch)
		return
	}
}

func (s *Supervisor) releaseStartPhaseLocked() {
	if !s.restartHold || s.barrier == nil {
		return
	}

	if !s.restartPhaseCompleteLocked() {
		return
	}

	if !s.barrier.ordered {
		for _, ch := range s.barrier.startGates {
			close(ch)
		}
		s.barrier.startGates = nil
		return
	}

	for s.barrier.nextStart < len(s.barrier.order) {
		w := s.barrier.order[s.barrier.nextStart]
		s.barrier.nextStart++

		if _, skipped := s.barrier.skipped[w]; skipped {
			continue
		}

		ch, ok := s.barrier.startGates[w]
		if !ok {
			continue
		}

		delete(s.barrier.startGates, w)
		close(ch)
		return
	}
}

func (s *Supervisor) releaseExecPhaseLocked() {
	if !s.restartHold || s.barrier == nil {
		return
	}

	if !s.startPhaseCompleteLocked() {
		return
	}

	for _, ch := range s.barrier.execGates {
		close(ch)
	}

	close(s.restartCh)
	s.restartCh = s.readyCh
	s.restartHold = false
	s.barrier = nil
}
