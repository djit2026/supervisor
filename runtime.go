package supervisor

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

type workerRuntime struct {
	spec         WorkerSpec
	state        workerState
	mu           sync.Mutex
	instanceID   int64
	restartTimes []time.Time
	metrics      workerMetrics
	ctx          context.Context
	cancel       context.CancelFunc
}

func (w *workerRuntime) setStatus(status Status) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.state.Status = status
	if status == StatusRunning {
		now := time.Now()
		w.state.StartedAt = now
		w.state.LastHeartbeat = now
	}
}

func (w *workerRuntime) updateExecution(err error, duration time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.state.ExecCount++
	w.state.LastLatency = duration
	w.state.LastError = err

	if err != nil {
		w.state.Status = StatusFailed
	}
}

func (w *workerRuntime) incrementRestart() {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.state.Restarts++
	w.state.LastRestart = time.Now()
}

func (w *workerRuntime) getRestarts() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state.Restarts
}

func (w *workerRuntime) isStopped() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state.Status == StatusStopped
}

func (w *workerRuntime) heartbeat() {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.state.LastHeartbeat = time.Now()
}

func (w *workerRuntime) isStuck(ttl time.Duration) bool {
	if ttl == 0 {
		return false
	}

	w.mu.Lock()
	last := w.state.LastHeartbeat
	w.mu.Unlock()

	return time.Since(last) > ttl
}

func (w *workerRuntime) nextInstance() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.instanceID++
	return w.instanceID
}

func (w *workerRuntime) newContext(parent context.Context) context.Context {
	ctx, cancel := context.WithCancel(parent)

	w.mu.Lock()
	w.ctx = ctx
	w.cancel = cancel
	w.mu.Unlock()

	return ctx
}

func (w *workerRuntime) cancelInstance() {
	w.mu.Lock()
	cancel := w.cancel
	w.mu.Unlock()

	if cancel != nil {
		cancel()
	}
}

func (w *workerRuntime) allowRestart() bool {
	spec := w.spec

	// If not configured → allow
	if spec.MaxRestartsInWindow == 0 || spec.RestartWindow == 0 {
		return true
	}

	now := time.Now()

	w.mu.Lock()
	defer w.mu.Unlock()

	// remove old timestamps
	var recent []time.Time
	for _, t := range w.restartTimes {
		if now.Sub(t) <= spec.RestartWindow {
			recent = append(recent, t)
		}
	}
	w.restartTimes = recent

	// check limit
	if len(w.restartTimes) >= spec.MaxRestartsInWindow {
		return false
	}

	// record restart
	w.restartTimes = append(w.restartTimes, now)

	return true
}

func (w *workerRuntime) metricsFail() {
	atomic.AddInt64(&w.metrics.Failures, 1)
}

func (w *workerRuntime) metricsSuccess() {
	atomic.AddInt64(&w.metrics.Success, 1)
}

func (w *workerRuntime) metricsRestart() {
	atomic.AddInt64(&w.metrics.Restarts, 1)
}

func (w *workerRuntime) metricsLatency(d time.Duration) {
	atomic.StoreInt64(&w.metrics.LastLatency, d.Nanoseconds())
}
