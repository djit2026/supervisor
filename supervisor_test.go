package supervisor

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestOneForOneRestartsFailedWorker(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls int32
	sup := NewSupervisor(WithStrategy(OneForOne))
	sup.Add(WorkerSpec{
		Name:    "worker",
		Restart: RestartOnFailure,
		Run: func(ctx context.Context, hb func()) error {
			if atomic.AddInt32(&calls, 1) == 1 {
				return errors.New("boom")
			}
			cancel()
			return nil
		},
	})

	done := startSupervisor(t, sup, ctx)
	waitFor(t, func() bool { return atomic.LoadInt32(&calls) >= 2 })
	cancel()
	waitDone(t, done)
}

func TestOneForAllRestartsCancelledWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var failedRuns int32
	var siblingRuns int32

	sup := NewSupervisor(WithStrategy(OneForAll))
	sup.Add(WorkerSpec{
		Name:    "failed",
		Restart: RestartOnFailure,
		Run: func(ctx context.Context, hb func()) error {
			if atomic.AddInt32(&failedRuns, 1) == 1 {
				fmt.Println("DEBUG failed: returning boom")
				return errors.New("boom")
			}

			fmt.Println("DEBUG failed: run 2 started, waiting for sibling")
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) && atomic.LoadInt32(&siblingRuns) < 2 {
				time.Sleep(time.Millisecond)
			}
			fmt.Printf("DEBUG failed: run 2 done, siblingRuns=%d, calling cancel\n", atomic.LoadInt32(&siblingRuns))
			cancel()
			return nil
		},
	})
	sup.Add(WorkerSpec{
		Name:    "sibling",
		Restart: RestartAlways,
		Run: func(ctx context.Context, hb func()) error {
			run := atomic.AddInt32(&siblingRuns, 1)
			fmt.Printf("DEBUG sibling: run %d started\n", run)
			<-ctx.Done()
			fmt.Printf("DEBUG sibling: run %d done (ctx cancelled)\n", run)
			return nil
		},
	})

	done := startSupervisor(t, sup, ctx)
	waitFor(t, func() bool { return atomic.LoadInt32(&siblingRuns) >= 2 })
	cancel()
	waitDone(t, done)
}

func TestOneForAllWaitsForAllWorkersBeforeRestarting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var failedRuns int32
	var siblingRuns int32
	var siblingStopped int32
	var earlyRestart int32

	sup := NewSupervisor(WithStrategy(OneForAll))
	sup.Add(WorkerSpec{
		Name:    "failed",
		Restart: RestartOnFailure,
		Run: func(ctx context.Context, hb func()) error {
			run := atomic.AddInt32(&failedRuns, 1)
			if run == 1 {
				waitFor(t, func() bool { return atomic.LoadInt32(&siblingRuns) == 1 })
				return errors.New("boom")
			}

			if atomic.LoadInt32(&siblingStopped) == 0 {
				atomic.StoreInt32(&earlyRestart, 1)
			}
			cancel()
			return nil
		},
	})
	sup.Add(WorkerSpec{
		Name:    "sibling",
		Restart: RestartAlways,
		Run: func(ctx context.Context, hb func()) error {
			atomic.AddInt32(&siblingRuns, 1)
			<-ctx.Done()
			time.Sleep(150 * time.Millisecond)
			atomic.StoreInt32(&siblingStopped, 1)
			return nil
		},
	})

	done := startSupervisor(t, sup, ctx)
	waitFor(t, func() bool { return atomic.LoadInt32(&failedRuns) >= 2 })
	cancel()
	waitDone(t, done)

	if atomic.LoadInt32(&earlyRestart) != 0 {
		t.Fatal("failed worker restarted before sibling fully stopped")
	}
}

func TestOneForAllEmitsLifecycleEventsBeforeExecution(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var failedRuns int32
	var siblingRuns int32
	sup := NewSupervisor(WithStrategy(OneForAll))
	events := sup.Subscribe()
	sup.Add(WorkerSpec{
		Name:    "failed",
		Restart: RestartOnFailure,
		Run: func(ctx context.Context, hb func()) error {
			run := atomic.AddInt32(&failedRuns, 1)
			if run == 1 {
				waitFor(t, func() bool { return atomic.LoadInt32(&siblingRuns) == 1 })
				return errors.New("boom")
			}

			<-ctx.Done()
			return nil
		},
	})
	sup.Add(WorkerSpec{
		Name:    "sibling",
		Restart: RestartAlways,
		Run: func(ctx context.Context, hb func()) error {
			run := atomic.AddInt32(&siblingRuns, 1)
			if run == 1 {
				<-ctx.Done()
				time.Sleep(150 * time.Millisecond)
				return nil
			}

			waitFor(t, func() bool { return atomic.LoadInt32(&failedRuns) == 2 })
			cancel()
			return nil
		},
	})

	done := startSupervisor(t, sup, ctx)
	waitFor(t, func() bool { return atomic.LoadInt32(&failedRuns) == 1 && atomic.LoadInt32(&siblingRuns) == 1 })

	restarted := 0
	startedAfterRestart := 0
	deadline := time.After(2 * time.Second)
	for restarted < 2 || startedAfterRestart < 2 {
		select {
		case event := <-events:
			if event.Type == EventRestarted {
				restarted++
			}
			if event.Type == EventStarted && event.Restarts > 0 {
				if restarted < 2 {
					t.Fatalf("worker %q started before all workers emitted restarted", event.Worker)
				}
				startedAfterRestart++
			}
		case <-deadline:
			t.Fatal("timed out waiting for restart lifecycle events")
		}
	}

	waitFor(t, func() bool { return atomic.LoadInt32(&failedRuns) >= 2 && atomic.LoadInt32(&siblingRuns) >= 2 })
	cancel()
	waitDone(t, done)
}

func TestRestForOneWaitsAndRestartsInOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var aRuns int32
	var bRuns int32
	var cRuns int32
	var cStopped int32
	var earlyRestart int32

	sup := NewSupervisor(WithStrategy(RestForOne))
	events := sup.Subscribe()
	sup.Add(WorkerSpec{
		Name:    "a",
		Restart: RestartAlways,
		Run: func(ctx context.Context, hb func()) error {
			atomic.AddInt32(&aRuns, 1)
			<-ctx.Done()
			return nil
		},
	})
	sup.Add(WorkerSpec{
		Name:    "b",
		Restart: RestartOnFailure,
		Run: func(ctx context.Context, hb func()) error {
			run := atomic.AddInt32(&bRuns, 1)
			if run == 1 {
				waitFor(t, func() bool { return atomic.LoadInt32(&cRuns) == 1 })
				return errors.New("boom")
			}

			if atomic.LoadInt32(&cStopped) == 0 {
				atomic.StoreInt32(&earlyRestart, 1)
			}
			<-ctx.Done()
			return nil
		},
	})
	sup.Add(WorkerSpec{
		Name:    "c",
		Restart: RestartAlways,
		Run: func(ctx context.Context, hb func()) error {
			run := atomic.AddInt32(&cRuns, 1)
			if run == 1 {
				<-ctx.Done()
				time.Sleep(150 * time.Millisecond)
				atomic.StoreInt32(&cStopped, 1)
				return nil
			}

			cancel()
			return nil
		},
	})

	done := startSupervisor(t, sup, ctx)
	waitFor(t, func() bool { return atomic.LoadInt32(&cRuns) >= 2 })
	cancel()
	waitDone(t, done)

	if atomic.LoadInt32(&earlyRestart) != 0 {
		t.Fatal("failed worker restarted before later worker fully stopped")
	}
	if atomic.LoadInt32(&aRuns) != 1 {
		t.Fatalf("worker before failed worker restarted: got %d runs, want 1", aRuns)
	}

	got := restartedWorkers(t, events, 2)
	want := []string{"b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("restart order = %v, want %v", got, want)
	}
}

func TestRestForOneDoesNotEmitRestartedBeforeBarrierRelease(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var bRuns int32
	var cRuns int32
	var cStopped int32

	sup := NewSupervisor(WithStrategy(RestForOne))
	events := sup.Subscribe()
	sup.Add(WorkerSpec{
		Name:    "b",
		Restart: RestartOnFailure,
		Run: func(ctx context.Context, hb func()) error {
			if atomic.AddInt32(&bRuns, 1) == 1 {
				waitFor(t, func() bool { return atomic.LoadInt32(&cRuns) == 1 })
				return errors.New("boom")
			}
			cancel()
			return nil
		},
	})
	sup.Add(WorkerSpec{
		Name:    "c",
		Restart: RestartAlways,
		Run: func(ctx context.Context, hb func()) error {
			if atomic.AddInt32(&cRuns, 1) == 1 {
				<-ctx.Done()
				time.Sleep(150 * time.Millisecond)
				atomic.StoreInt32(&cStopped, 1)
				return nil
			}
			return nil
		},
	})

	done := startSupervisor(t, sup, ctx)
	waitFor(t, func() bool { return atomic.LoadInt32(&bRuns) == 1 && atomic.LoadInt32(&cRuns) == 1 })

	deadline := time.After(2 * time.Second)
	for atomic.LoadInt32(&cStopped) == 0 {
		select {
		case event := <-events:
			if event.Type == EventRestarted {
				t.Fatalf("got restarted event for %q before barrier release", event.Worker)
			}
		case <-deadline:
			t.Fatal("timed out waiting for delayed worker to stop")
		}
	}

	waitFor(t, func() bool { return atomic.LoadInt32(&bRuns) >= 2 })
	cancel()
	waitDone(t, done)
}

func TestContextCancellationIsNotCountedAsFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var started int32
	sup := NewSupervisor()
	sup.Add(WorkerSpec{
		Name:    "worker",
		Restart: RestartOnFailure,
		Run: func(ctx context.Context, hb func()) error {
			atomic.StoreInt32(&started, 1)
			<-ctx.Done()
			return ctx.Err()
		},
	})

	done := startSupervisor(t, sup, ctx)
	waitFor(t, func() bool { return atomic.LoadInt32(&started) == 1 })
	cancel()
	waitDone(t, done)

	metrics := sup.Metrics()
	if len(metrics) != 1 {
		t.Fatalf("got %d metrics, want 1", len(metrics))
	}
	if metrics[0].Failures != 0 {
		t.Fatalf("got %d failures, want 0", metrics[0].Failures)
	}
}

func startSupervisor(t *testing.T, sup *Supervisor, ctx context.Context) <-chan error {
	t.Helper()

	done := make(chan error, 1)
	go func() {
		done <- sup.Start(ctx)
	}()
	return done
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}

	t.Fatal("condition was not met before timeout")
}

func waitDone(t *testing.T, done <-chan error) {
	t.Helper()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("supervisor returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not stop before timeout")
	}
}

func restartedWorkers(t *testing.T, events <-chan Event, count int) []string {
	t.Helper()

	var workers []string
	deadline := time.After(2 * time.Second)
	for len(workers) < count {
		select {
		case event := <-events:
			if event.Type == EventRestarted {
				workers = append(workers, event.Worker)
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %d restarted events, got %v", count, workers)
		}
	}
	return workers
}

// TestBarrierWatchdogUnblocksOnTimeout verifies that a worker which never
// stops does not permanently deadlock the supervisor. The barrier watchdog
// should fire and emit EventBarrierTimeout. (Bug 1.1)
func TestBarrierWatchdogUnblocksOnTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var failedRuns int32
	// forceStop is closed by the test after the watchdog fires, to unblock
	// the truly stuck sibling so the supervisor can shut down cleanly.
	forceStop := make(chan struct{})

	// Very short barrier timeout so the test doesn't take long.
	sup := NewSupervisor(
		WithStrategy(OneForAll),
		WithBarrierTimeout(200*time.Millisecond),
	)
	events := sup.Subscribe()

	// Failing worker triggers restart.
	sup.Add(WorkerSpec{
		Name:    "trigger",
		Restart: RestartOnFailure,
		Run: func(ctx context.Context, hb func()) error {
			if atomic.AddInt32(&failedRuns, 1) == 1 {
				return errors.New("boom")
			}
			cancel()
			return nil
		},
	})

	// Sibling that truly ignores ctx cancellation until forceStop is closed.
	sup.Add(WorkerSpec{
		Name:    "stuck",
		Restart: RestartAlways,
		Run: func(ctx context.Context, hb func()) error {
			select {
			case <-forceStop:
				return nil
			case <-ctx.Done():
				// Only yield on global shutdown (forceStop already closed).
				<-forceStop
				return nil
			}
		},
	})

	done := startSupervisor(t, sup, ctx)

	// Wait for EventBarrierTimeout.
	deadline := time.After(3 * time.Second)
	gotTimeout := false
	for !gotTimeout {
		select {
		case ev := <-events:
			if ev.Type == EventBarrierTimeout {
				gotTimeout = true
			}
		case <-deadline:
			close(forceStop)
			t.Fatal("timed out waiting for EventBarrierTimeout")
		}
	}

	// Unblock the stuck sibling, then shut down.
	close(forceStop)
	cancel()
	waitDone(t, done)
}

// TestNoChannelReplacementRace verifies that concurrent restarts do not panic
// or deadlock due to stale channel references. (Bug 1.2)
func TestNoChannelReplacementRace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var runs int32
	sup := NewSupervisor(WithStrategy(OneForOne))
	sup.Add(WorkerSpec{
		Name:    "worker",
		Restart: RestartOnFailure,
		Run: func(ctx context.Context, hb func()) error {
			n := atomic.AddInt32(&runs, 1)
			if n < 5 {
				return errors.New("boom")
			}
			cancel()
			return nil
		},
	})

	done := startSupervisor(t, sup, ctx)
	waitFor(t, func() bool { return atomic.LoadInt32(&runs) >= 5 })
	cancel()
	waitDone(t, done)
}

// TestRestartCoalescing verifies that multiple concurrent failure events from
// different workers produce only a single restart cycle, not overlapping ones.
// (Bug 1.4)
func TestRestartCoalescing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var restartCycles int32 // count EventRestarted events seen

	sup := NewSupervisor(WithStrategy(OneForAll))
	events := sup.Subscribe()

	var aRuns, bRuns int32

	sup.Add(WorkerSpec{
		Name:    "a",
		Restart: RestartOnFailure,
		Run: func(ctx context.Context, hb func()) error {
			atomic.AddInt32(&aRuns, 1)
			if atomic.LoadInt32(&aRuns) == 1 {
				return errors.New("a failed")
			}
			// Second run: return immediately.
			return nil
		},
	})
	sup.Add(WorkerSpec{
		Name:    "b",
		Restart: RestartOnFailure,
		Run: func(ctx context.Context, hb func()) error {
			n := atomic.AddInt32(&bRuns, 1)
			if n == 1 {
				return errors.New("b failed")
			}
			// Second run: cancel supervisor and exit.
			cancel()
			return nil
		},
	})

	done := startSupervisor(t, sup, ctx)

	// Collect EventRestarted events while waiting for the restart cycle to
	// complete. We only require bRuns>=2 since b is guaranteed to increment
	// it before calling cancel().
	deadline := time.After(3 * time.Second)
	for atomic.LoadInt32(&bRuns) < 2 {
		select {
		case ev := <-events:
			if ev.Type == EventRestarted {
				atomic.AddInt32(&restartCycles, 1)
			}
		case <-deadline:
			t.Fatal("timed out waiting for restarts")
		}
	}

	cancel()
	waitDone(t, done)

	// With coalescing, only 1 restart cycle (≤2 EventRestarted events total).
	got := atomic.LoadInt32(&restartCycles)
	if got > 2 {
		t.Fatalf("restart coalescing failed: got %d EventRestarted events, want ≤2", got)
	}
}

// TestCancelInstanceBoundToID verifies that cancelling a worker instance does
// not affect a newer instance that has already started. (Bug 1.6)
func TestCancelInstanceBoundToID(t *testing.T) {
	var latestID int64 // tracks latest instance ID seen by the worker

	w := &workerRuntime{
		workerID: "test",
		spec:     WorkerSpec{Name: "test"},
		gateCh:   make(chan struct{}, 1),
	}

	ctx1, _ := w.nextInstance(context.Background())
	id1 := w.currentInstanceID()
	atomic.StoreInt64(&latestID, id1)

	// Simulate starting a second instance (id1 is now stale).
	ctx2, _ := w.nextInstance(context.Background())
	id2 := w.currentInstanceID()
	atomic.StoreInt64(&latestID, id2)

	// Cancelling with the stale id1 should NOT cancel ctx2.
	w.cancelInstance(id1)
	if ctx2.Err() != nil {
		t.Fatal("stale cancelInstance(id1) incorrectly cancelled ctx2")
	}
	// ctx1 was replaced by nextInstance so it may or may not be cancelled;
	// what matters is ctx2 is safe.
	_ = ctx1
}

// TestRestartReasonTracked verifies that the RestartReason is correctly set
// and surfaces in events and snapshots. (Improvement 3.9)
func TestRestartReasonTracked(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var runs int32
	sup := NewSupervisor(WithStrategy(OneForOne))
	events := sup.Subscribe()

	sup.Add(WorkerSpec{
		Name:    "worker",
		Restart: RestartOnFailure,
		Run: func(ctx context.Context, hb func()) error {
			if atomic.AddInt32(&runs, 1) == 1 {
				return errors.New("failure")
			}
			cancel()
			return nil
		},
	})

	done := startSupervisor(t, sup, ctx)

	var restartEvent Event
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Type == EventRestarted {
				restartEvent = ev
				goto found
			}
		case <-deadline:
			t.Fatal("timed out waiting for EventRestarted")
		}
	}
found:
	if restartEvent.RestartReason != ReasonFailure {
		t.Fatalf("got restart reason %v, want ReasonFailure", restartEvent.RestartReason)
	}

	cancel()
	waitDone(t, done)
}

// TestHeartbeatWatchdogDetectsStuck verifies that the background watchdog
// cancels a stuck worker (no heartbeat) and triggers a restart. (Improvement 3.7)
func TestHeartbeatWatchdogDetectsStuck(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var runs int32
	sup := NewSupervisor(
		WithStrategy(OneForOne),
		WithHeartbeatInterval(10*time.Millisecond),
	)

	sup.Add(WorkerSpec{
		Name:         "worker",
		Restart:      RestartOnFailure,
		HeartbeatTTL: 50 * time.Millisecond,
		Run: func(ctx context.Context, hb func()) error {
			n := atomic.AddInt32(&runs, 1)
			if n == 1 {
				// First run: deliberately do NOT call hb() — simulate stuck.
				// Block until the watchdog cancels our context, then return
				// nil so the supervisor's heartbeat check classifies it as
				// a timeout (not a normal cancel).
				<-ctx.Done()
				return nil
			}
			// Second run: clean exit to let the test finish.
			cancel()
			return nil
		},
	})

	done := startSupervisor(t, sup, ctx)
	waitFor(t, func() bool { return atomic.LoadInt32(&runs) >= 2 })
	cancel()
	waitDone(t, done)

	metrics := sup.Metrics()
	if metrics[0].TimeoutCount < 1 {
		t.Fatalf("TimeoutCount = %d, want >= 1", metrics[0].TimeoutCount)
	}
}

// TestDumpState verifies DumpState returns a coherent snapshot of all workers.
// (Improvement 3.8)
func TestDumpState(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sup := NewSupervisor(WithStrategy(OneForAll))
	sup.Add(WorkerSpec{
		Name:    "alpha",
		Restart: RestartAlways,
		Run: func(ctx context.Context, hb func()) error {
			<-ctx.Done()
			return nil
		},
	})
	sup.Add(WorkerSpec{
		Name:    "beta",
		Restart: RestartAlways,
		Run: func(ctx context.Context, hb func()) error {
			<-ctx.Done()
			return nil
		},
	})

	done := startSupervisor(t, sup, ctx)
	waitFor(t, func() bool {
		snap := sup.Snapshot()
		for _, w := range snap {
			if w.Status != StatusRunning {
				return false
			}
		}
		return true
	})

	dump := sup.DumpState()
	if dump.Strategy != "OneForAll" {
		t.Fatalf("Strategy = %q, want OneForAll", dump.Strategy)
	}
	if len(dump.Workers) != 2 {
		t.Fatalf("got %d workers in dump, want 2", len(dump.Workers))
	}
	for _, w := range dump.Workers {
		if w.Status != StatusRunning {
			t.Fatalf("worker %q status = %v, want Running", w.Name, w.Status)
		}
	}

	cancel()
	waitDone(t, done)
}

// TestMetricsConsistency verifies that failure counts and success counts do
// not drift between Metrics() and Snapshot() calls. (Design Flaw 2.4)
func TestMetricsConsistency(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var runs int32
	sup := NewSupervisor(WithStrategy(OneForOne))
	sup.Add(WorkerSpec{
		Name:    "worker",
		Restart: RestartOnFailure,
		Run: func(ctx context.Context, hb func()) error {
			if atomic.AddInt32(&runs, 1) == 1 {
				return errors.New("boom")
			}
			cancel()
			return nil
		},
	})

	done := startSupervisor(t, sup, ctx)
	waitFor(t, func() bool { return atomic.LoadInt32(&runs) >= 2 })
	cancel()
	waitDone(t, done)

	metrics := sup.Metrics()
	snaps := sup.Snapshot()

	if len(metrics) != 1 || len(snaps) != 1 {
		t.Fatal("unexpected number of workers")
	}

	if metrics[0].Failures != snaps[0].Failures {
		t.Fatalf("Failures: Metrics()=%d vs Snapshot()=%d", metrics[0].Failures, snaps[0].Failures)
	}
	if metrics[0].Success != snaps[0].Success {
		t.Fatalf("Success: Metrics()=%d vs Snapshot()=%d", metrics[0].Success, snaps[0].Success)
	}
}

// TestPanicErrorKind verifies that a panicking worker gets ErrorKind="panic"
// and PanicCount incremented. (Improvement 3.6)
func TestPanicErrorKind(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var runs int32
	sup := NewSupervisor(WithStrategy(OneForOne))
	events := sup.Subscribe()

	sup.Add(WorkerSpec{
		Name:    "panicker",
		Restart: RestartOnFailure,
		Run: func(ctx context.Context, hb func()) error {
			if atomic.AddInt32(&runs, 1) == 1 {
				panic("something went wrong")
			}
			cancel()
			return nil
		},
	})

	done := startSupervisor(t, sup, ctx)

	var failEvent Event
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Type == EventFailed {
				failEvent = ev
				goto foundPanic
			}
		case <-deadline:
			t.Fatal("timed out waiting for EventFailed from panic")
		}
	}
foundPanic:
	if failEvent.ErrorKind != "panic" {
		t.Fatalf("ErrorKind = %q, want \"panic\"", failEvent.ErrorKind)
	}

	cancel()
	waitDone(t, done)

	metrics := sup.Metrics()
	if metrics[0].PanicCount < 1 {
		t.Fatalf("PanicCount = %d, want >= 1", metrics[0].PanicCount)
	}
}
