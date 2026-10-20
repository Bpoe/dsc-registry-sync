package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
)

func platformRun(args []string) int {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	service, err := svc.IsWindowsService()
	if err != nil {
		log.Error("cannot detect Windows service context")
		return 1
	}
	if !service {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		return boundedRun(ctx, func() int { return run(ctx, args, os.Stderr) }, os.Stderr, shutdownTimeout)
	}
	events, err := eventlog.Open("dscregsyncd")
	if err != nil {
		log.Error("cannot open service event log")
		return 1
	}
	defer events.Close()
	output := eventWriter{events}
	handler := &serviceHandler{
		run: func(ctx context.Context, ready func()) int { return runReady(ctx, args, output, ready) },
		log: slog.New(slog.NewJSONHandler(output, nil)), timeout: shutdownTimeout,
	}
	if err := svc.Run("dscregsyncd", handler); err != nil {
		handler.log.Error("Windows service dispatcher failed")
		return 1
	}
	return handler.code
}

type eventWriter struct{ events *eventlog.Log }

func (w eventWriter) Write(p []byte) (int, error) {
	if err := w.events.Info(1, string(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}

type serviceHandler struct {
	run     func(context.Context, func()) int
	log     *slog.Logger
	timeout time.Duration
	code    int
}

func (h *serviceHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, statuses chan<- svc.Status) (bool, uint32) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	done := make(chan int, 1)
	go func() { done <- h.run(ctx, func() { close(ready) }) }()
	state := svc.Status{State: svc.StartPending, CheckPoint: 1, WaitHint: 30000}
	statuses <- state
	ticks := time.NewTicker(time.Second)
	defer ticks.Stop()
	var deadline <-chan time.Time
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-ready:
			ready = nil
			if state.State == svc.StartPending {
				state = svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
				statuses <- state
			}
		case request, ok := <-requests:
			if ok && request.Cmd == svc.Interrogate {
				statuses <- state
			}
			if !ok || request.Cmd == svc.Stop || request.Cmd == svc.Shutdown {
				if !ok {
					requests = nil
				}
				if state.State != svc.StopPending {
					cancel()
					state = svc.Status{State: svc.StopPending, CheckPoint: 1, WaitHint: uint32(h.timeout.Milliseconds())}
					statuses <- state
					timer = time.NewTimer(h.timeout)
					deadline = timer.C
				}
			}
		case <-ticks.C:
			if state.State == svc.StartPending || state.State == svc.StopPending {
				state.CheckPoint++
				statuses <- state
			}
		case h.code = <-done:
			return h.code != 0, uint32(h.code)
		case <-deadline:
			h.code = 1
			h.log.Error("shutdown deadline exceeded; cleanup incomplete")
			return true, 1
		}
	}
}
