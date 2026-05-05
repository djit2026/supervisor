package supervisor

import "time"

// Status describes the current lifecycle state of a worker.
type Status int

const (
	StatusStarting   Status = iota
	StatusRunning
	StatusRestarting // worker is stopped, awaiting restart coordination
	StatusStopping   // worker is being gracefully stopped
	StatusStopped
	StatusFailed
)

func (s Status) String() string {
	switch s {
	case StatusStarting:
		return "STARTING"
	case StatusRunning:
		return "RUNNING"
	case StatusRestarting:
		return "RESTARTING"
	case StatusStopping:
		return "STOPPING"
	case StatusStopped:
		return "STOPPED"
	case StatusFailed:
		return "FAILED"
	default:
		return "UNKNOWN"
	}
}

// RestartReason records why a worker restart was triggered.
type RestartReason int

const (
	ReasonNone     RestartReason = iota
	ReasonFailure                // worker returned a non-nil error
	ReasonPanic                  // worker function panicked
	ReasonTimeout                // heartbeat TTL exceeded
	ReasonStrategy               // cancelled by OneForAll / RestForOne
	ReasonThrottle               // restart rate limit exceeded
)

func (r RestartReason) String() string {
	switch r {
	case ReasonFailure:
		return "FAILURE"
	case ReasonPanic:
		return "PANIC"
	case ReasonTimeout:
		return "TIMEOUT"
	case ReasonStrategy:
		return "STRATEGY"
	case ReasonThrottle:
		return "THROTTLE"
	default:
		return "NONE"
	}
}

type workerState struct {
	Name          string
	Status        Status
	StartedAt     time.Time
	LastRestart   time.Time
	Restarts      int
	LastError     error
	ErrorKind     string        // "panic" | "timeout" | "error" | ""
	RestartReason RestartReason // reason for the most recent restart
	ExecCount     int64
	LastLatency   time.Duration
	LastHeartbeat time.Time
}
