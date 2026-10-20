package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

func platformRun(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return boundedRun(ctx, func() int { return run(ctx, args, os.Stderr) }, os.Stderr, shutdownTimeout)
}
