package supervisor

import (
	"sync/atomic"
	"time"
)

// WorkerSnapshot is a read-only copy of a worker's current state and counters.
type WorkerSnapshot struct {
	Name          string
	WorkerID      string
	Status        Status
	StartedAt     time.Time
	LastRestart   time.Time
	Restarts      int64
	LastError     error
	ErrorKind     string
	RestartReason RestartReason
	ExecCount     int64
	LastLatency   time.Duration
	LastHeartbeat time.Time
	Failures      int64
	Success       int64
	PanicCount    int64
	TimeoutCount  int64
}

// WorkerMetricsSnapshot is a read-only copy of a worker's counters.
type WorkerMetricsSnapshot struct {
	Name             string
	Restarts         int64
	Failures         int64
	Success          int64
	PanicCount       int64
	TimeoutCount     int64
	StrategyRestarts int64
	LastLatency      time.Duration
}

// SupervisorDump is a rich debug snapshot of the entire supervisor state.
// Returned by DumpState().
type SupervisorDump struct {
	Strategy      string
	Workers       []WorkerSnapshot
	DroppedEvents int64
}

// Snapshot returns a consistent copy of every worker's state and metrics.
func (s *Supervisor) Snapshot() []WorkerSnapshot {
	s.mu.Lock()
	workers := make([]*workerRuntime, len(s.workers))
	copy(workers, s.workers)
	s.mu.Unlock()

	result := make([]WorkerSnapshot, 0, len(workers))

	for _, w := range workers {
		w.mu.Lock()
		stateCopy := w.state
		wid := w.workerID
		w.mu.Unlock()

		result = append(result, WorkerSnapshot{
			Name:          stateCopy.Name,
			WorkerID:      wid,
			Status:        stateCopy.Status,
			StartedAt:     stateCopy.StartedAt,
			LastRestart:   stateCopy.LastRestart,
			Restarts:      int64(stateCopy.Restarts),
			LastError:     stateCopy.LastError,
			ErrorKind:     stateCopy.ErrorKind,
			RestartReason: stateCopy.RestartReason,
			ExecCount:     stateCopy.ExecCount,
			LastLatency:   stateCopy.LastLatency,
			LastHeartbeat: stateCopy.LastHeartbeat,
			Failures:      atomic.LoadInt64(&w.metrics.Failures),
			Success:       atomic.LoadInt64(&w.metrics.Success),
			PanicCount:    atomic.LoadInt64(&w.metrics.PanicCount),
			TimeoutCount:  atomic.LoadInt64(&w.metrics.TimeoutCount),
		})
	}

	return result
}

// DumpState returns a rich debug snapshot of the supervisor and all workers.
// (Improvement 3.8)
func (s *Supervisor) DumpState() SupervisorDump {
	s.mu.Lock()
	strategy := s.strategy
	s.mu.Unlock()

	strategyStr := "OneForOne"
	switch strategy {
	case OneForAll:
		strategyStr = "OneForAll"
	case RestForOne:
		strategyStr = "RestForOne"
	}

	return SupervisorDump{
		Strategy:      strategyStr,
		Workers:       s.Snapshot(),
		DroppedEvents: atomic.LoadInt64(&s.droppedEvents),
	}
}
