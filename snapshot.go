package supervisor

import (
	"sync/atomic"
	"time"
)

// WorkerSnapshot is a read-only copy of a worker's current state and counters.
type WorkerSnapshot struct {
	Name          string
	Status        Status
	StartedAt     time.Time
	LastRestart   time.Time
	Restarts      int64
	LastError     error
	ExecCount     int64
	LastLatency   time.Duration
	LastHeartbeat time.Time
	Failures      int64
	Success       int64
}

// WorkerMetricsSnapshot is a read-only copy of a worker's counters.
type WorkerMetricsSnapshot struct {
	Name        string
	Restarts    int64
	Failures    int64
	Success     int64
	LastLatency time.Duration
}

// Snapshot returns a consistent copy of every worker's state.
func (s *Supervisor) Snapshot() []WorkerSnapshot {
	s.mu.Lock()
	workers := make([]*workerRuntime, len(s.workers))
	copy(workers, s.workers)
	s.mu.Unlock()

	result := make([]WorkerSnapshot, 0, len(workers))

	for _, w := range workers {
		w.mu.Lock()
		stateCopy := w.state
		w.mu.Unlock()

		result = append(result, WorkerSnapshot{
			Name:          stateCopy.Name,
			Status:        stateCopy.Status,
			StartedAt:     stateCopy.StartedAt,
			LastRestart:   stateCopy.LastRestart,
			Restarts:      int64(stateCopy.Restarts),
			LastError:     stateCopy.LastError,
			ExecCount:     stateCopy.ExecCount,
			LastLatency:   stateCopy.LastLatency,
			LastHeartbeat: stateCopy.LastHeartbeat,
			Failures:      atomic.LoadInt64(&w.metrics.Failures),
			Success:       atomic.LoadInt64(&w.metrics.Success),
		})
	}

	return result
}
