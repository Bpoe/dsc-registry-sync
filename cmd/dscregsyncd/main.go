package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Bpoe/dsc-registry-sync/internal/config"
	"github.com/Bpoe/dsc-registry-sync/internal/registry"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, output io.Writer) int {
	log := slog.New(slog.NewJSONHandler(output, nil))
	flags := flag.NewFlagSet("dscregsyncd", flag.ContinueOnError)
	// flag errors echo user input; only help output is safe to print verbatim.
	flags.SetOutput(io.Discard)
	path := flags.String("config", config.DefaultPath(), "path to the service JSON configuration")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.PrintDefaults()
			return 0
		}
		log.Error("invalid command line; use -help")
		return 2
	}
	if flags.NArg() != 0 {
		log.Error("unexpected positional arguments; use -help")
		return 2
	}
	cfg, err := config.Load(*path)
	if err != nil {
		log.Error("startup failed", "error", err)
		return 1
	}
	key, err := config.ReadAPIKey(cfg.APIKeyFile)
	if err != nil {
		log.Error("startup failed", "error", err)
		return 1
	}
	etag, err := registry.ReadETag(cfg.ConfigDir)
	if err != nil {
		log.Error("startup failed", "error", err)
		return 1
	}
	client := registry.NewClient(cfg.RegistryURL, key, time.Duration(cfg.RequestTimeoutSeconds)*time.Second)
	defer client.Close()
	log.Info("daemon started", "mode", "read-only", "committed_etag_present", etag != "")
	poll(ctx, client, etag, time.Duration(cfg.PollFallbackSeconds)*time.Second, log, wait)
	log.Info("daemon stopped")
	return 0
}

func poll(ctx context.Context, client *registry.Client, etag string, fallback time.Duration,
	log *slog.Logger, sleep func(context.Context, time.Duration) error,
) {
	schedule := registry.Schedule{Fallback: fallback}
	for ctx.Err() == nil {
		response, err := client.Fetch(ctx, etag)
		if ctx.Err() != nil {
			return
		}
		delay := schedule.Next(response.Header, err != nil, time.Now(), rand.Float64())
		switch {
		case err != nil:
			log.Warn("desired-state poll failed", "status", response.StatusCode, "error", err, "retry_in", delay)
		case response.Collection == nil:
			log.Info("desired state unchanged", "status", response.StatusCode, "next_poll_in", delay)
		default:
			log.Info("desired-state collection validated; publication deferred",
				"status", response.StatusCode, "configuration_count", len(response.Collection.Configurations),
				"next_poll_in", delay)
		}
		// Do not promote a received ETag, even in memory. Only PR 2's successful
		// collection publication can commit a validator for subsequent polls.
		if sleep(ctx, delay) != nil {
			return
		}
	}
}

func wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
