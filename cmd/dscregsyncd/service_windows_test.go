package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
)

func TestServiceLifecycle(t *testing.T) {
	for _, command := range []svc.Cmd{svc.Stop, svc.Shutdown} {
		requests := make(chan svc.ChangeRequest)
		statuses := make(chan svc.Status, 16)
		done := make(chan int, 1)
		handler := serviceHandler{
			run: func(ctx context.Context, ready func()) int {
				ready()
				<-ctx.Done()
				return 0
			},
			log: slog.New(slog.NewJSONHandler(io.Discard, nil)), timeout: time.Second,
		}
		go func() {
			failed, code := handler.Execute(nil, requests, statuses)
			if failed || code != 0 {
				done <- 1
			} else {
				done <- 0
			}
		}()
		expectState(t, statuses, svc.StartPending)
		expectState(t, statuses, svc.Running)
		requests <- svc.ChangeRequest{Cmd: svc.Interrogate}
		expectState(t, statuses, svc.Running)
		requests <- svc.ChangeRequest{Cmd: command}
		expectState(t, statuses, svc.StopPending)
		select {
		case code := <-done:
			if code != 0 {
				t.Fatal("clean service stop failed")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("service stop hung")
		}
	}
}

func TestServiceFailureAndDeadline(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		requests := make(chan svc.ChangeRequest)
		close(requests)
		statuses := make(chan svc.Status, 16)
		release := make(chan struct{})
		handler := serviceHandler{
			run: func(context.Context, func()) int {
				if blocked {
					<-release
				}
				return 2
			},
			log: slog.New(slog.NewJSONHandler(io.Discard, nil)), timeout: 20 * time.Millisecond,
		}
		failed, code := handler.Execute(nil, requests, statuses)
		close(release)
		if !failed || code == 0 || handler.code == 0 {
			t.Fatal("service failure reported success")
		}
		for len(statuses) > 0 {
			if (<-statuses).State == svc.Running {
				t.Fatal("service became ready before startup validation")
			}
		}
	}
}

func TestServiceStopDuringStartup(t *testing.T) {
	requests := make(chan svc.ChangeRequest)
	close(requests)
	statuses := make(chan svc.Status, 16)
	handler := serviceHandler{
		run: func(ctx context.Context, ready func()) int {
			<-ctx.Done()
			ready()
			return 0
		},
		log: slog.New(slog.NewJSONHandler(io.Discard, nil)), timeout: time.Second,
	}
	if failed, code := handler.Execute(nil, requests, statuses); failed || code != 0 {
		t.Fatal("startup cancellation failed")
	}
	for len(statuses) > 0 {
		if (<-statuses).State == svc.Running {
			t.Fatal("late readiness undid stop-pending")
		}
	}
}

func expectState(t *testing.T, statuses <-chan svc.Status, expected svc.State) {
	t.Helper()
	select {
	case state := <-statuses:
		if state.State != expected {
			t.Fatalf("state %v, want %v", state.State, expected)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("missing service status")
	}
}
