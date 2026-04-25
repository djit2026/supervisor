package supervisor

// RestartPolicy controls whether an individual worker should restart.
type RestartPolicy int

const (
	// RestartAlways restarts the worker after any completed run.
	RestartAlways RestartPolicy = iota
	// RestartOnFailure restarts the worker only after errors or panics.
	RestartOnFailure
	// RestartNever stops the worker after its current run.
	RestartNever
)

func (s *Supervisor) shouldRestart(w *workerRuntime, err error) bool {
	spec := w.spec

	// Max restart limit
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
