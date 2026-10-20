package main

import (
	"context"
	"io"
	"log/slog"
	"time"
)

const shutdownTimeout = 30 * time.Second

func boundedRun(ctx context.Context, worker func() int, output io.Writer, timeout time.Duration) int {
	done := make(chan int, 1)
	go func() { done <- worker() }()
	select {
	case code := <-done:
		return code
	case <-ctx.Done():
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case code := <-done:
		return code
	case <-timer.C:
		slog.New(slog.NewJSONHandler(output, nil)).Error("shutdown deadline exceeded; cleanup incomplete")
		return 1
	}
}
