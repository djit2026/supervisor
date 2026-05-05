package supervisor

import (
	"context"
	"time"
)

// WorkerFunc is the function executed by a supervised worker.
type WorkerFunc func(ctx context.Context, hb func()) error

// WorkerSpec configures a supervised worker.
type WorkerSpec struct {
	// Name is a human-readable label; used as the stable ID when WorkerID is empty.
	Name string

	// WorkerID is the stable identifier used internally for all maps and tracking.
	// Falls back to Name when empty.
	WorkerID string

	Run                 WorkerFunc
	Restart             RestartPolicy
	Backoff             BackoffStrategy
	MaxRestarts         int
	HeartbeatTTL        time.Duration
	MaxRestartsInWindow int
	RestartWindow       time.Duration
}

// id returns the stable identifier for the worker.
func (s WorkerSpec) id() string {
	if s.WorkerID != "" {
		return s.WorkerID
	}
	return s.Name
}
