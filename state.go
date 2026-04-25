package supervisor

import "time"

// Status describes the current lifecycle state of a worker.
type Status int

const (
	StatusStarting Status = iota
	StatusRunning
	StatusStopped
	StatusFailed
)

func (s Status) String() string {
	switch s {
	case StatusStarting:
		return "STARTING"
	case StatusRunning:
		return "RUNNING"
	case StatusStopped:
		return "STOPPED"
	case StatusFailed:
		return "FAILED"
	default:
		return "UNKNOWN"
	}
}

type workerState struct {
	Name          string
	Status        Status
	StartedAt     time.Time
	LastRestart   time.Time
	Restarts      int
	LastError     error
	ExecCount     int64
	LastLatency   time.Duration
	LastHeartbeat time.Time
}
