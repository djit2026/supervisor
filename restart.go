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
