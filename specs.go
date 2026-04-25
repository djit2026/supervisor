package supervisor

import (
	"context"
	"time"
)

// WorkerFunc is the function executed by a supervised worker.
type WorkerFunc func(ctx context.Context, hb func()) error

// WorkerSpec configures a supervised worker.
type WorkerSpec struct {
	Name                string
	Run                 WorkerFunc
	Restart             RestartPolicy
	Backoff             BackoffStrategy
	MaxRestarts         int
	HeartbeatTTL        time.Duration
	MaxRestartsInWindow int
	RestartWindow       time.Duration
}
