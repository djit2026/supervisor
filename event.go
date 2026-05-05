package supervisor

import "time"

// EventType identifies a worker lifecycle event.
type EventType int

const (
	// EventStarted is emitted before a worker's Run function is called.
	EventStarted EventType = iota
	// EventStopped is emitted after a worker instance exits or is cancelled.
	EventStopped
	// EventRestarted is emitted after a worker passes restart policy and coordination.
	EventRestarted
	// EventFailed is emitted when a worker returns an error or panics.
	EventFailed
	// EventThrottled is emitted when restart limits prevent another restart.
	EventThrottled
	// EventBarrierTimeout is emitted when the restart barrier watchdog fires.
	// The supervisor force-releases all blocked workers and continues.
	EventBarrierTimeout
)

// Event describes a worker lifecycle transition.
type Event struct {
	Worker     string
	InstanceID int64
	Type       EventType
	Err        error

	// ErrorKind classifies the error: "panic", "timeout", "error", or "".
	ErrorKind string

	// RestartReason records why the worker was restarted.
	RestartReason RestartReason

	Time     time.Time
	Restarts int
	Latency  time.Duration
}

func (t EventType) String() string {
	switch t {
	case EventStarted:
		return "STARTED"
	case EventStopped:
		return "STOPPED"
	case EventRestarted:
		return "RESTARTED"
	case EventFailed:
		return "FAILED"
	case EventThrottled:
		return "THROTTLED"
	case EventBarrierTimeout:
		return "BARRIER_TIMEOUT"
	default:
		return "UNKNOWN"
	}
}
