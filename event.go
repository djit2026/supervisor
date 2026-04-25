package supervisor

import "time"

type EventType int

const (
	EventStarted EventType = iota
	EventStopped
	EventRestarted
	EventFailed
	EventThrottled
)

type Event struct {
	Worker     string
	InstanceID int64
	Type       EventType
	Err        error
	Time       time.Time

	Restarts int
	Latency  time.Duration
}
