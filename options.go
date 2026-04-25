package supervisor

type Option func(*Supervisor)

func WithEventBuffer(size int) Option {
	return func(s *Supervisor) {
		s.eventBuffer = size
	}
}

func WithStrategy(s Strategy) Option {
	return func(sup *Supervisor) {
		sup.strategy = s
	}
}
