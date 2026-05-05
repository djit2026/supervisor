package main

import (
	"context"
	"fmt"
	"time"

	"github.com/djit2026/supervisor"
)

func main() {
	sup := supervisor.NewSupervisor(
		supervisor.WithStrategy(supervisor.RestForOne),
	)

	mustAdd(sup, supervisor.WorkerSpec{
		Name:    "A",
		Restart: supervisor.RestartAlways,
		Run: func(ctx context.Context, hb func()) error {
			for {
				select {
				case <-ctx.Done():
					fmt.Println("[A] stopped")
					return nil
				default:
					hb()
					fmt.Println("[A] working")
					time.Sleep(time.Second)
				}
			}
		},
	})

	mustAdd(sup, supervisor.WorkerSpec{
		Name:    "B",
		Restart: supervisor.RestartOnFailure,
		Run: func(ctx context.Context, hb func()) error {
			fmt.Println("[B] running")
			time.Sleep(2 * time.Second)
			return fmt.Errorf("B failed")
		},
	})

	mustAdd(sup, supervisor.WorkerSpec{
		Name:    "C",
		Restart: supervisor.RestartAlways,
		Run: func(ctx context.Context, hb func()) error {
			for {
				select {
				case <-ctx.Done():
					fmt.Println("[C] stopped")
					return nil
				default:
					hb()
					fmt.Println("[C] working")
					time.Sleep(time.Second)
				}
			}
		},
	})

	go func() {
		for event := range sup.Subscribe() {
			fmt.Printf("[EVENT] worker=%s type=%s err=%v\n", event.Worker, event.Type, event.Err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := sup.Start(ctx); err != nil {
		panic(err)
	}
}

func mustAdd(sup *supervisor.Supervisor, spec supervisor.WorkerSpec) {
	if err := sup.Add(spec); err != nil {
		panic(err)
	}
}
