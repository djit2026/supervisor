package supervisor

import "time"

type Status int

const (
	StatusStarting Status = iota
	StatusRunning
	StatusStopped
	StatusFailed
)

type WorkerState struct {
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
