package supervisor

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
func WithStrategy(s Strategy) Option {
	return func(sup *Supervisor) {
		sup.strategy = s
	}
}
