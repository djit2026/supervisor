package supervisor

type RestartPolicy int

const (
	RestartAlways RestartPolicy = iota
	RestartOnFailure
	RestartNever
)

func (s *Supervisor) shouldRestart(w *workerRuntime, err error) bool {
	spec := w.spec

	// 🔴 Max restart limit
	if spec.MaxRestarts > 0 && w.getRestarts() >= spec.MaxRestarts {
		return false
	}

	switch spec.Restart {

	case RestartAlways:
		return true

	case RestartOnFailure:
		return err != nil

	case RestartNever:
		return false
	}

	return false
}
