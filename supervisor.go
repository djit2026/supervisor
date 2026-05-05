package supervisor

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Strategy controls which workers are restarted when one worker fails.
type Strategy int

const (
	// OneForOne restarts only the failed worker.
	OneForOne Strategy = iota
	// OneForAll restarts all active workers when one worker fails.
	OneForAll
	// RestForOne restarts the failed worker and all workers registered after it.
	RestForOne
)

const (
	defaultBarrierTimeout    = 30 * time.Second
	defaultHeartbeatInterval = time.Second
)

// restartRequest is posted to the centralized restart loop when a worker
// under OneForAll or RestForOne strategy fails.
type restartRequest struct {
	trigger *workerRuntime
	reason  RestartReason
}

// Supervisor manages a set of worker goroutines.
type Supervisor struct {
	mu      sync.Mutex
	workers []*workerRuntime
	// workersByID provides O(1) lookup by stable string ID. (Bug 1.5 fix)
	workersByID map[string]*workerRuntime

	wg      sync.WaitGroup
	started bool

	events        chan Event
	eventBuffer   int
	droppedEvents int64 // atomic counter for dropped events

	ctx    context.Context
	cancel context.CancelFunc

	strategy Strategy

	// restartCh carries at most one pending coordinated restart at a time.
	// Duplicates are coalesced via restartPending. (Bug 1.4 fix, Improvement 3.4)
	restartCh      chan restartRequest
	restartPending bool // guarded by mu

	barrierTimeout    time.Duration // max wait for all workers to stop (Bug 1.1 fix)
	heartbeatInterval time.Duration // polling interval for heartbeat watchdog
}

// NewSupervisor creates a supervisor with the supplied options.
func NewSupervisor(opts ...Option) *Supervisor {
	s := &Supervisor{
		eventBuffer:       100,
		barrierTimeout:    defaultBarrierTimeout,
		heartbeatInterval: defaultHeartbeatInterval,
		workersByID:       make(map[string]*workerRuntime),
	}

	for _, opt := range opts {
		opt(s)
	}

	s.events = make(chan Event, s.eventBuffer)
	s.restartCh = make(chan restartRequest, 1)

	return s
}

// Add registers a worker. It must be called before Start.
func (s *Supervisor) Add(spec WorkerSpec) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return fmt.Errorf("cannot add workers after start")
	}
	if spec.Name == "" {
		return fmt.Errorf("worker name is required")
	}
	if spec.Run == nil {
		return fmt.Errorf("worker %q run function is required", spec.Name)
	}

	id := spec.id()
	if _, exists := s.workersByID[id]; exists {
		return fmt.Errorf("worker %q already registered", id)
	}

	rt := &workerRuntime{
		workerID: id,
		spec:     spec,
		state: workerState{
			Name:   spec.Name,
			Status: StatusStarting,
		},
		// gateCh is created once and never replaced. (Bug 1.2 fix)
		// Capacity 1: pre-filled so the first run starts immediately.
		gateCh: make(chan struct{}, 1),
	}
	rt.gateCh <- struct{}{}

	s.workers = append(s.workers, rt)
	s.workersByID[id] = rt
	return nil
}

// Start runs all registered workers and blocks until they all exit.
func (s *Supervisor) Start(parent context.Context) error {
	s.mu.Lock()

	if s.started {
		s.mu.Unlock()
		return fmt.Errorf("already started")
	}

	if parent == nil {
		parent = context.Background()
	}

	s.ctx, s.cancel = context.WithCancel(parent)
	s.started = true

	workers := make([]*workerRuntime, len(s.workers))
	copy(workers, s.workers)

	s.mu.Unlock()

	// Start the centralized restart coordinator.
	s.wg.Add(1)
	go s.restartLoop()

	// Start the background heartbeat watchdog.
	s.wg.Add(1)
	go s.heartbeatWatchdog()

	// Start all worker goroutines.
	for _, w := range workers {
		s.wg.Add(1)
		go s.runWorker(w)
	}

	s.wg.Wait()

	close(s.events)

	return nil
}

// Stop cancels the supervisor context, signalling all workers to stop.
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

// Subscribe returns the lifecycle event stream.
func (s *Supervisor) Subscribe() <-chan Event {
	return s.events
}

// Metrics returns a copy of each worker's atomic counters.
func (s *Supervisor) Metrics() []WorkerMetricsSnapshot {
	s.mu.Lock()
	workers := make([]*workerRuntime, len(s.workers))
	copy(workers, s.workers)
	s.mu.Unlock()

	out := make([]WorkerMetricsSnapshot, 0, len(workers))
	for _, w := range workers {
		out = append(out, WorkerMetricsSnapshot{
			Name:             w.spec.Name,
			Restarts:         atomic.LoadInt64(&w.metrics.Restarts),
			Failures:         atomic.LoadInt64(&w.metrics.Failures),
			Success:          atomic.LoadInt64(&w.metrics.Success),
			PanicCount:       atomic.LoadInt64(&w.metrics.PanicCount),
			TimeoutCount:     atomic.LoadInt64(&w.metrics.TimeoutCount),
			StrategyRestarts: atomic.LoadInt64(&w.metrics.StrategyRestarts),
			LastLatency: time.Duration(
				atomic.LoadInt64(&w.metrics.LastLatency),
			),
		})
	}

	return out
}

func (s *Supervisor) runWorker(w *workerRuntime) {
	defer s.wg.Done()

	for {
		// Wait for the gate to open. The first iteration runs immediately
		// because Add() pre-fills gateCh. For coordinated restarts, the
		// restartLoop opens the gate only after all workers in the restart
		// set have fully stopped. (Bug 1.2 fix — gate never replaced)
		w.setAtGate(true)
		select {
		case <-w.gateCh:
			w.setAtGate(false)
		case <-s.ctx.Done():
			w.setAtGate(false)
			w.setStatus(StatusStopped)
			s.emit(Event{
				Worker: w.spec.Name,
				Type:   EventStopped,
				Time:   time.Now(),
			})
			return
		}

		if s.ctx.Err() != nil {
			w.setAtGate(false)
			w.setStatus(StatusStopped)
			s.emit(Event{
				Worker: w.spec.Name,
				Type:   EventStopped,
				Time:   time.Now(),
			})
			return
		}

		// Check if the supervisor has decided this worker should stop permanently
		// (restart policy exhausted / rate limited during a coordinated cycle).
		w.mu.Lock()
		perm := w.permanentStop
		w.mu.Unlock()
		if perm {
			w.setStatus(StatusStopped)
			return
		}

		// Create a fresh execution context for this instance.
		ctx, instanceID := w.nextInstance(s.ctx)

		w.setStatus(StatusRunning)
		s.emit(Event{
			Worker:     w.spec.Name,
			InstanceID: instanceID,
			Type:       EventStarted,
			Time:       time.Now(),
			Restarts:   w.getRestarts(),
		})

		start := time.Now()
		err, errorKind := s.safeRun(ctx, w)
		wasCancelled := ctx.Err() != nil
		duration := time.Since(start)

		// Heartbeat check: if the worker was cancelled (by the watchdog)
		// and is stuck, treat it as a timeout failure — not as a coordinated
		// restart signal. We also allow this path for non-cancelled exits.
		isHeartbeatTimeout := w.isStuck(w.spec.HeartbeatTTL)
		if isHeartbeatTimeout {
			err = fmt.Errorf("worker stuck: no heartbeat")
			errorKind = "timeout"
			w.metricsTimeout()
			// Treat as a non-cancelled failure so the restart logic below
			// handles it as a self-restart (not a coordinated restart).
			wasCancelled = false
		}

		// update execution state
		w.updateExecution(err, errorKind, duration)

		// metrics
		if err != nil && !wasCancelled {
			w.metricsFail()
		} else {
			w.metricsSuccess()
		}
		w.metricsLatency(duration)

		// emit failure event
		if err != nil && !wasCancelled {
			s.emit(Event{
				Worker:     w.spec.Name,
				InstanceID: instanceID,
				Type:       EventFailed,
				Err:        err,
				ErrorKind:  errorKind,
				Time:       time.Now(),
				Restarts:   w.getRestarts(),
				Latency:    duration,
			})
		}

		// Global shutdown: context cancelled at supervisor level.
		if wasCancelled && s.ctx.Err() != nil {
			w.setStatus(StatusStopped)
			s.emit(Event{
				Worker:     w.spec.Name,
				InstanceID: instanceID,
				Type:       EventStopped,
				Time:       time.Now(),
			})
			return
		}

		// Worker was cancelled by restartLoop for coordinated restart.
		// Loop back — atGate will be set at the top of the next iteration
		// so restartLoop can detect we are ready.
		if wasCancelled {
			w.setStatus(StatusRestarting)
			s.emit(Event{
				Worker:     w.spec.Name,
				InstanceID: instanceID,
				Type:       EventStopped,
				Time:       time.Now(),
			})
			continue
		}

		// Worker exited on its own (not cancelled).

		// For coordinated strategies: post to restartLoop which will stop
		// siblings, wait for all to stop, then open all gates.
		// For OneForOne: check restart policy and open own gate directly.
		if s.strategy == OneForOne {
			// OneForOne: no siblings affected, handle inline.
			if !s.shouldRestart(w, err) {
				w.setStatus(StatusStopped)
				s.emit(Event{
					Worker:     w.spec.Name,
					InstanceID: instanceID,
					Type:       EventStopped,
					Time:       time.Now(),
				})
				return
			}

			if !w.allowRestart() {
				w.setStatus(StatusStopped)
				w.setRestartReason(ReasonThrottle)
				s.emit(Event{
					Worker:        w.spec.Name,
					InstanceID:    instanceID,
					Type:          EventThrottled,
					Err:           fmt.Errorf("restart throttled"),
					RestartReason: ReasonThrottle,
					Time:          time.Now(),
				})
				return
			}

			reason := classifyReason(errorKind)
			w.setRestartReason(reason)
			w.incrementRestart()
			w.metricsRestart()

			s.emit(Event{
				Worker:        w.spec.Name,
				InstanceID:    instanceID,
				Type:          EventRestarted,
				RestartReason: reason,
				Time:          time.Now(),
				Restarts:      w.getRestarts(),
			})

			// Apply backoff before signalling.
			if !s.applyBackoff(w, instanceID, ctx) {
				return
			}

			s.openGate(w)
			continue
		}

		// OneForAll / RestForOne: coordinated restart is only triggered on
		// FAILURE (err != nil). Normal completion restarts the worker inline
		// (like OneForOne) without disturbing siblings. This matches OTP
		// semantics where the strategy only fires when a worker crashes.
		if err == nil {
			// Normal completion: restart inline if policy allows.
			if !s.shouldRestart(w, nil) {
				w.setStatus(StatusStopped)
				s.emit(Event{
					Worker:     w.spec.Name,
					InstanceID: instanceID,
					Type:       EventStopped,
					Time:       time.Now(),
				})
				return
			}

			if !w.allowRestart() {
				w.setStatus(StatusStopped)
				w.setRestartReason(ReasonThrottle)
				s.emit(Event{
					Worker:        w.spec.Name,
					InstanceID:    instanceID,
					Type:          EventThrottled,
					Err:           fmt.Errorf("restart throttled"),
					RestartReason: ReasonThrottle,
					Time:          time.Now(),
				})
				return
			}

			w.setRestartReason(ReasonNone)
			w.incrementRestart()
			w.metricsRestart()
			s.emit(Event{
				Worker:     w.spec.Name,
				InstanceID: instanceID,
				Type:       EventRestarted,
				Time:       time.Now(),
				Restarts:   w.getRestarts(),
			})

			if !s.applyBackoff(w, instanceID, ctx) {
				return
			}
			s.openGate(w)
			continue
		}

		// Failure path: post to centralized restart loop.
		// The worker will loop back to gateCh and wait.
		reason := classifyReason(errorKind)
		w.setRestartReason(reason)

		// Apply backoff before the coordinated restart so siblings aren't
		// held up needlessly by one worker's backoff.
		if !s.applyBackoff(w, instanceID, ctx) {
			return
		}

		s.postRestart(w, reason)

		// Loop back to gateCh — restartLoop opens it after coordination.
	}
}

// applyBackoff sleeps for the configured backoff duration.
// Returns false if the supervisor shut down during the sleep (caller should return).
func (s *Supervisor) applyBackoff(w *workerRuntime, instanceID int64, ctx context.Context) bool {
	if w.spec.Backoff == nil {
		return true
	}

	delay := w.spec.Backoff.Next(w.getRestarts())
	if delay <= 0 {
		return true
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-s.ctx.Done():
		w.setStatus(StatusStopped)
		s.emit(Event{
			Worker:     w.spec.Name,
			InstanceID: instanceID,
			Type:       EventStopped,
			Time:       time.Now(),
		})
		return false
	}
}

// postRestart coalesces restart requests to the centralized loop. (Bug 1.4 fix)
func (s *Supervisor) postRestart(trigger *workerRuntime, reason RestartReason) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.restartPending {
		// A restart cycle is already queued; discard duplicate.
		return
	}
	s.restartPending = true
	s.restartCh <- restartRequest{trigger: trigger, reason: reason}
}

func (s *Supervisor) safeRun(ctx context.Context, w *workerRuntime) (err error, kind string) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
			kind = "panic"
			w.metricsPanic()
		}
	}()

	runErr := w.spec.Run(ctx, w.heartbeat)
	if runErr != nil {
		return runErr, "error"
	}
	return nil, ""
}

func (s *Supervisor) restartLoop() {
	defer s.wg.Done()

	for {
		select {
		case <-s.ctx.Done():
			return

		case req := <-s.restartCh:
			// Keep restartPending=true until the full cycle completes so
			// that any failures during the cycle are coalesced, not queued
			// as a second overlapping cycle. (Bug 1.4 fix)
			s.runRestartCycle(req)

			s.mu.Lock()
			s.restartPending = false
			s.mu.Unlock()
		}
	}
}

// runRestartCycle executes one full coordinated restart.
func (s *Supervisor) runRestartCycle(req restartRequest) {
	s.mu.Lock()
	workers := make([]*workerRuntime, len(s.workers))
	copy(workers, s.workers)
	strategy := s.strategy
	s.mu.Unlock()

	// Determine which workers to include in the restart set.
	var toRestart []*workerRuntime
	switch strategy {
	case OneForAll:
		toRestart = workers
	case RestForOne:
		found := false
		for _, w := range workers {
			if w == req.trigger {
				found = true
			}
			if found {
				toRestart = append(toRestart, w)
			}
		}
	default:
		return
	}

	// Mark non-trigger workers with the strategy reason.
	for _, w := range toRestart {
		if w != req.trigger {
			w.setRestartReason(ReasonStrategy)
			w.metricsStrategyRestart()
		}
	}

	// Stop all workers in the restart set.
	// The trigger has already exited; cancel it anyway (no-op safe via instanceID check).
	instanceIDs := make(map[string]int64, len(toRestart))
	for _, w := range toRestart {
		id := w.currentInstanceID()
		instanceIDs[w.workerID] = id
		if w != req.trigger {
			w.setStatus(StatusStopping)
			w.cancelInstance(id)
		}
	}

	// Wait for all workers in the restart set to have reached the gate
	// (i.e., they have finished their current run and are blocked waiting).
	// Uses the atGate flag rather than isStopped to correctly handle the
	// trigger worker which exited without cancellation. (Bug 1.1 fix)
	deadline := time.Now().Add(s.barrierTimeout)
	allStoppedCh := make(chan struct{})

	go func() {
		for _, w := range toRestart {
			for !w.isAtGate() {
				if time.Now().After(deadline) {
					return
				}
				time.Sleep(2 * time.Millisecond)
			}
		}
		close(allStoppedCh)
	}()

	timedOut := false
	select {
	case <-allStoppedCh:
		// All workers cleanly stopped.
	case <-time.After(time.Until(deadline)):
		// Watchdog fired: force-release. (Bug 1.1 fix)
		timedOut = true
		s.emit(Event{
			Type: EventBarrierTimeout,
			Time: time.Now(),
		})
	case <-s.ctx.Done():
		return
	}
	_ = timedOut

	// Now open gates in the correct order, but only after emitting EventRestarted
	// for all restarting workers first. This preserves the ordering invariant:
	// EventRestarted is always emitted before the corresponding EventStarted.

	// Determine which workers will actually restart (vs. exit due to policy).
	type workerDecision struct {
		w         *workerRuntime
		willReset bool
	}
	decisions := make([]workerDecision, 0, len(toRestart))
	for _, w := range toRestart {
		w.mu.Lock()
		lastErr := w.state.LastError
		w.mu.Unlock()

		willRestart := s.shouldRestart(w, lastErr) && w.allowRestart()
		decisions = append(decisions, workerDecision{w: w, willReset: willRestart})
	}

	// Emit EventRestarted for all workers that WILL restart, before opening any gate.
	for _, d := range decisions {
		if !d.willReset {
			continue
		}
		d.w.incrementRestart()
		d.w.metricsRestart()
		s.emit(Event{
			Worker:        d.w.spec.Name,
			InstanceID:    instanceIDs[d.w.workerID],
			Type:          EventRestarted,
			RestartReason: req.reason,
			Time:          time.Now(),
			Restarts:      d.w.getRestarts(),
		})
	}

	// Now open gates / finalise workers.
	for _, d := range decisions {
		if d.willReset {
			// Open the gate; the worker is blocked on it and will restart.
			s.openGate(d.w)
		} else {
			// Worker won't restart: set permanentStop so it exits when it
			// wakes from the gate, then open the gate to wake it.
			d.w.mu.Lock()
			d.w.permanentStop = true
			d.w.mu.Unlock()

			if !s.shouldRestart(d.w, d.w.state.LastError) {
				s.emit(Event{
					Worker:     d.w.spec.Name,
					InstanceID: instanceIDs[d.w.workerID],
					Type:       EventStopped,
					Time:       time.Now(),
				})
			} else {
				// Rate-limited.
				d.w.setRestartReason(ReasonThrottle)
				s.emit(Event{
					Worker:        d.w.spec.Name,
					InstanceID:    instanceIDs[d.w.workerID],
					Type:          EventThrottled,
					Err:           fmt.Errorf("restart throttled"),
					RestartReason: ReasonThrottle,
					Time:          time.Now(),
				})
			}

			s.openGate(d.w)
		}
	}
}

// openGate signals a worker's gate channel without blocking.
// The channel has capacity 1; if already full, the worker will get
// the existing token (no loss of signal).
func (s *Supervisor) openGate(w *workerRuntime) {
	select {
	case w.gateCh <- struct{}{}:
	default:
	}
}

func (s *Supervisor) heartbeatWatchdog() {
	defer s.wg.Done()

	ticker := time.NewTicker(s.heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return

		case <-ticker.C:
			s.mu.Lock()
			workers := make([]*workerRuntime, len(s.workers))
			copy(workers, s.workers)
			s.mu.Unlock()

			for _, w := range workers {
				ttl := w.spec.HeartbeatTTL
				if ttl == 0 {
					continue
				}

				w.mu.Lock()
				status := w.state.Status
				w.mu.Unlock()

				if status != StatusRunning {
					continue
				}

				if w.isStuck(ttl) {
					id := w.currentInstanceID()
					// Cancel the stuck instance; runWorker will detect
					// the timeout on its next evaluation.
					w.cancelInstance(id)
				}
			}
		}
	}
}

func (s *Supervisor) shouldRestart(w *workerRuntime, err error) bool {
	spec := w.spec

	if spec.MaxRestarts > 0 && w.getRestarts() >= spec.MaxRestarts {
		return false
	}

	switch spec.Restart {
	case RestartAlways:
		return true
	case RestartOnFailure:
		return err != nil
	case RestartNever:
		return false
	}

	return false
}

func (s *Supervisor) emit(e Event) {
	if s.events == nil {
		return
	}

	select {
	case s.events <- e:
	default:
		atomic.AddInt64(&s.droppedEvents, 1)
	}
}

// classifyReason maps an errorKind string to a RestartReason.
func classifyReason(errorKind string) RestartReason {
	switch errorKind {
	case "panic":
		return ReasonPanic
	case "timeout":
		return ReasonTimeout
	default:
		return ReasonFailure
	}
}
