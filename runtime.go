package supervisor

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// workerRuntime holds all mutable state for a single supervised worker.
// The supervisor is the sole owner of state transitions; workerRuntime
// only exposes fine-grained helpers.
type workerRuntime struct {
	// spec is immutable after Add().
	spec WorkerSpec

	// workerID is the stable string identifier (never a pointer).
	workerID string

	// mu protects all fields below.
	mu    sync.Mutex
	state workerState

	// instanceID is the monotonic instance counter, incremented each restart.
	instanceID int64

	// instanceCtx / instanceCancel are the context for the current execution
	// instance. Both are replaced atomically under mu for each new instance.
	instanceCtx    context.Context
	instanceCancel context.CancelFunc

	// restartTimes tracks recent restart timestamps for rate-limiting.
	restartTimes []time.Time

	// metrics counters are updated via atomic operations for lock-free reads.
	metrics workerMetrics

	// gateCh is created once (in Add) and is used by the supervisor to signal
	// that the worker may proceed after a coordinated restart. It is NEVER
	// replaced — only drained and re-signalled. (Bug 1.2 fix)
	gateCh chan struct{}

	// atGate is true when the worker goroutine is blocked waiting on gateCh.
	// Used by the heartbeat watchdog to skip workers that aren't running.
	atGate bool

	// permanentStop is set by the supervisor when this worker should exit
	// permanently (e.g., policy says no more restarts). After waking from
	// gateCh, the worker will exit cleanly without running again.
	permanentStop bool

	// stopping is set to true when the supervisor has decided this worker
	// instance should stop (e.g. for coordinated restart or final shutdown).
	// Reads and writes are under mu.
	stopping bool
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

func (w *workerRuntime) setRestartReason(r RestartReason) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.state.RestartReason = r
}

func (w *workerRuntime) updateExecution(err error, errorKind string, duration time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.state.ExecCount++
	w.state.LastLatency = duration
	w.state.LastError = err
	w.state.ErrorKind = errorKind

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

// isAtGate reports whether the worker goroutine is currently blocked waiting
// at its gate channel.
func (w *workerRuntime) isAtGate() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.atGate
}

// setAtGate marks whether the worker is currently blocking at its gate.
func (w *workerRuntime) setAtGate(v bool) {
	w.mu.Lock()
	w.atGate = v
	w.mu.Unlock()
}

// isStopped reports whether the worker's current execution instance has
// already exited. Uses instanceCtx.Err() rather than Status, because Status
// may still be "Running" mid-transition. (Bug 1.3 fix)
func (w *workerRuntime) isStopped() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.instanceCtx == nil || w.instanceCtx.Err() != nil
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

// nextInstance increments and returns the new instanceID, and creates a fresh
// child context bound to parent. Returns the new context and instanceID.
// (Combines old nextInstance + newContext into one atomic operation.)
func (w *workerRuntime) nextInstance(parent context.Context) (context.Context, int64) {
	ctx, cancel := context.WithCancel(parent)

	w.mu.Lock()
	// If the worker was requested to stop before its first instance even
	// started (e.g. rapid failure of another worker triggered a cycle),
	// pre-cancel its first context so it immediately exits and joins the cycle.
	if w.instanceID == 0 && w.stopping {
		cancel()
	}

	w.instanceID++
	id := w.instanceID
	w.instanceCtx = ctx
	w.instanceCancel = cancel
	w.stopping = false
	w.mu.Unlock()

	return ctx, id
}

// cancelInstance cancels the context of the given instance only if the
// current instanceID still matches. This prevents cancelling the wrong
// (already-replaced) instance. (Bug 1.6 fix)
func (w *workerRuntime) cancelInstance(id int64) {
	w.mu.Lock()
	if w.instanceID != id {
		// stale cancel request — a new instance is already running
		w.mu.Unlock()
		return
	}
	cancel := w.instanceCancel
	w.stopping = true
	w.mu.Unlock()

	if cancel != nil {
		cancel()
	}
}

// currentInstanceID returns the current instance ID without locking (safe
// for reading when the caller already holds appropriate ordering guarantees).
func (w *workerRuntime) currentInstanceID() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.instanceID
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

func (w *workerRuntime) metricsPanic() {
	atomic.AddInt64(&w.metrics.PanicCount, 1)
}

func (w *workerRuntime) metricsTimeout() {
	atomic.AddInt64(&w.metrics.TimeoutCount, 1)
}

func (w *workerRuntime) metricsStrategyRestart() {
	atomic.AddInt64(&w.metrics.StrategyRestarts, 1)
}
