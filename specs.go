package supervisor

import (
	"context"
	"time"
)

type WorkerFunc func(ctx context.Context, hb func()) error

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
