package supervisor

import (
	"context"
	"errors"
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
				return errors.New("boom")
			}

			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) && atomic.LoadInt32(&siblingRuns) < 2 {
				time.Sleep(time.Millisecond)
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
