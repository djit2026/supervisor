package supervisor

type workerMetrics struct {
	Restarts    int64
	Failures    int64
	Success     int64
	LastLatency int64 // in nanoseconds
}
