# Goroutines Supervisor

A small Go supervisor for running and coordinating groups of goroutines with restart policies, restart strategies, lifecycle events, backoff, heartbeats, metrics, and snapshots.

It is inspired by supervision trees: each worker is declared with a `WorkerSpec`, and the supervisor decides what to do when a worker fails.

## Features

- Worker restart policies: always, on failure, or never
- Supervision strategies: `OneForOne`, `OneForAll`, and `RestForOne`
- Coordinated restart barriers for group strategies
- Lifecycle events for started, stopped, failed, restarted, and throttled workers
- Optional exponential backoff
- Restart throttling within a time window
- Heartbeat-based stuck worker detection
- Metrics and state snapshots
- Panic recovery inside worker functions

## Installation

This repository currently uses the module path:

```bash
go get supervisor
```

For local development, import it as:

```go
import "supervisor"
```

If you publish this module, replace the module path in `go.mod` with your repository URL, then import that path instead.

## Quick Start

```go
package main

import (
	"context"
	"fmt"
	"time"

	"supervisor"
)

func main() {
	sup := supervisor.NewSupervisor(
		supervisor.WithStrategy(supervisor.OneForAll),
	)

	sup.Add(supervisor.WorkerSpec{
		Name:    "api",
		Restart: supervisor.RestartOnFailure,
		Run: func(ctx context.Context, hb func()) error {
			fmt.Println("api started")
			time.Sleep(2 * time.Second)
			return fmt.Errorf("api failed")
		},
	})

	sup.Add(supervisor.WorkerSpec{
		Name:    "cache",
		Restart: supervisor.RestartAlways,
		Run: func(ctx context.Context, hb func()) error {
			for {
				select {
				case <-ctx.Done():
					fmt.Println("cache stopped")
					return nil
				default:
					hb()
					fmt.Println("cache working")
					time.Sleep(time.Second)
				}
			}
		},
	})

	go func() {
		for event := range sup.Subscribe() {
			fmt.Printf("worker=%s event=%v err=%v\n", event.Worker, event.Type, event.Err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := sup.Start(ctx); err != nil {
		panic(err)
	}
}
```

Run the included example:

```bash
go run ./examples/basic
```

## Core Concepts

### Supervisor

Create a supervisor with options:

```go
sup := supervisor.NewSupervisor(
	supervisor.WithStrategy(supervisor.RestForOne),
	supervisor.WithEventBuffer(256),
)
```

Add workers before calling `Start`:

```go
sup.Add(supervisor.WorkerSpec{
	Name:    "worker",
	Restart: supervisor.RestartOnFailure,
	Run: func(ctx context.Context, hb func()) error {
		// do work
		return nil
	},
})
```

`Start(ctx)` blocks until the supervisor context is cancelled or all worker goroutines exit.

```go
ctx, cancel := context.WithCancel(context.Background())
defer cancel()

err := sup.Start(ctx)
```

You can also stop a running supervisor with:

```go
_ = sup.Stop()
```

### WorkerSpec

```go
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
```

`Run` receives:

- `ctx`: cancelled when the worker should stop
- `hb`: heartbeat callback for stuck-worker detection

Workers should respect `ctx.Done()` and return promptly when cancelled.

## Restart Policies

Each worker controls whether its own execution should be restarted:

| Policy | Behavior |
| --- | --- |
| `RestartAlways` | Restart after success, failure, or strategy cancellation |
| `RestartOnFailure` | Restart only when `Run` returns an error or panics |
| `RestartNever` | Do not restart |

Panics are recovered and treated as worker failures.

## Supervision Strategies

The supervisor strategy controls which workers are stopped when a worker fails.

### OneForOne

Only the failed worker is restarted.

Use this when workers are independent.

```go
sup := supervisor.NewSupervisor(supervisor.WithStrategy(supervisor.OneForOne))
```

### OneForAll

When one worker fails, all active workers are stopped. The supervisor waits until every affected worker has fully exited before allowing any restart.

Restart lifecycle is synchronized:

```text
FAILED
all affected workers STOPPED
all affected workers RESTARTED
all affected workers STARTED
all affected workers enter Run()
```

Use this when workers share state or must be restarted as a group.

### RestForOne

When one worker fails, that worker and all workers added after it are stopped and restarted. Workers added before the failed worker continue running.

For affected workers, restart lifecycle preserves supervisor order:

```text
A added first
B added second
C added third

B fails
B and C stop
B restarts before C
B starts before C
B and C enter Run() only after the start phase is complete
```

Use this when later workers depend on earlier workers.

## Lifecycle Events

Subscribe before or after adding workers:

```go
events := sup.Subscribe()
go func() {
	for event := range events {
		fmt.Println(event.Worker, event.Type, event.Err)
	}
}()
```

Event types:

| Event | Meaning |
| --- | --- |
| `EventStarted` | Worker instance was started |
| `EventStopped` | Worker instance stopped |
| `EventRestarted` | Worker passed restart policy and restart barrier |
| `EventFailed` | Worker returned an error or panicked |
| `EventThrottled` | Restart was denied by throttling |

For coordinated strategies, events are ordered so `EventRestarted` is emitted before `EventStarted`, and `Run()` is called only after the relevant restart/start barriers are released.

## Backoff

Attach a backoff strategy to a worker:

```go
sup.Add(supervisor.WorkerSpec{
	Name:    "retrying-worker",
	Restart: supervisor.RestartOnFailure,
	Backoff: supervisor.ExponentialBackoff{
		Min:    100 * time.Millisecond,
		Max:    5 * time.Second,
		Factor: 2,
	},
	Run: func(ctx context.Context, hb func()) error {
		return fmt.Errorf("temporary failure")
	},
})
```

You can provide your own backoff by implementing:

```go
type BackoffStrategy interface {
	Next(retry int) time.Duration
}
```

## Restart Limits

Stop restarting after an absolute limit:

```go
sup.Add(supervisor.WorkerSpec{
	Name:        "limited",
	Restart:     supervisor.RestartOnFailure,
	MaxRestarts: 3,
	Run:         runWorker,
})
```

Throttle restarts within a rolling window:

```go
sup.Add(supervisor.WorkerSpec{
	Name:                "throttled",
	Restart:             supervisor.RestartOnFailure,
	MaxRestartsInWindow: 5,
	RestartWindow:       time.Minute,
	Run:                 runWorker,
})
```

When throttled, the worker emits `EventThrottled` and stops.

## Heartbeats

Workers can call `hb()` to report liveness. If `HeartbeatTTL` is set and the worker does not heartbeat within that duration, the supervisor treats it as stuck after `Run` returns.

```go
sup.Add(supervisor.WorkerSpec{
	Name:         "heartbeat-worker",
	Restart:      supervisor.RestartOnFailure,
	HeartbeatTTL: 5 * time.Second,
	Run: func(ctx context.Context, hb func()) error {
		for {
			select {
			case <-ctx.Done():
				return nil
			default:
				hb()
				time.Sleep(time.Second)
			}
		}
	},
})
```

## Metrics and Snapshots

Metrics provide counters and latency:

```go
for _, m := range sup.Metrics() {
	fmt.Printf("%s restarts=%d failures=%d success=%d latency=%s\n",
		m.Name,
		m.Restarts,
		m.Failures,
		m.Success,
		m.LastLatency,
	)
}
```

Snapshots expose current worker state:

```go
for _, state := range sup.Snapshot() {
	fmt.Printf("%s status=%v restarts=%d lastErr=%v\n",
		state.Name,
		state.Status,
		state.Restarts,
		state.LastError,
	)
}
```

## Testing

Run the full test suite:

```bash
go test ./...
```

Run without cache:

```bash
go test -count=1 ./...
```

## Notes

- Add all workers before calling `Start`; adding workers after start panics.
- `Start` closes the event channel when all worker goroutines have exited.
- Event delivery is buffered and non-blocking. If the event buffer is full, events may be dropped.
- Workers should cooperate with cancellation. A worker that ignores `ctx.Done()` can delay coordinated restarts.
- For `OneForAll` and `RestForOne`, coordinated restart semantics depend on affected workers returning from `Run` after cancellation.
