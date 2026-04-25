package supervisor

import "time"

type WorkerSnapshot struct {
	Name        string
	Restarts    int64
	Failures    int64
	Success     int64
	LastLatency time.Duration
}

func (s *Supervisor) Snapshot() []WorkerState {
	s.mu.Lock()
	workers := make([]*workerRuntime, len(s.workers))
	copy(workers, s.workers)
	s.mu.Unlock()

	result := make([]WorkerState, 0, len(workers))

	for _, w := range workers {
		w.mu.Lock()
		stateCopy := w.state
		w.mu.Unlock()

		result = append(result, stateCopy)
	}

	return result
}
