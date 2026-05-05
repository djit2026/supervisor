package supervisor

import "time"

// Option is a functional option for configuring a Supervisor.
type Option func(*Supervisor)

// WithEventBuffer sets the lifecycle event channel buffer size.
func WithEventBuffer(size int) Option {
	return func(s *Supervisor) {
		if size < 0 {
			return
		}
		s.eventBuffer = size
	}
}

// WithStrategy sets the restart strategy used when a worker fails.
func WithStrategy(st Strategy) Option {
	return func(sup *Supervisor) {
		sup.strategy = st
	}
}

// WithBarrierTimeout sets the maximum time the supervisor will wait for all
// workers to stop during a coordinated restart before force-releasing the
// barrier. An EventBarrierTimeout event is emitted when this fires.
// Default: 30s.
func WithBarrierTimeout(d time.Duration) Option {
	return func(s *Supervisor) {
		if d > 0 {
			s.barrierTimeout = d
		}
	}
}

// WithHeartbeatInterval sets the polling interval for the background heartbeat
// watchdog loop. Default: 1s.
func WithHeartbeatInterval(d time.Duration) Option {
	return func(s *Supervisor) {
		if d > 0 {
			s.heartbeatInterval = d
		}
	}
}
