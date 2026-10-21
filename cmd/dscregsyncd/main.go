package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"time"

	"github.com/Bpoe/dsc-registry-sync/internal/config"
	"github.com/Bpoe/dsc-registry-sync/internal/publish"
	"github.com/Bpoe/dsc-registry-sync/internal/registry"
	"github.com/Bpoe/dsc-registry-sync/internal/results"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "register" {
		os.Exit(runRegister(context.Background(), os.Args[2:], os.Stdin, os.Stdout, os.Stderr, terminalToken))
	}
	os.Exit(platformRun(os.Args[1:]))
}

func run(ctx context.Context, args []string, output io.Writer) int {
	return runReady(ctx, args, output, func() {})
}

func runReady(ctx context.Context, args []string, output io.Writer, ready func()) int {
	if len(args) > 0 && args[0] == "register" {
		return runRegister(ctx, args[1:], os.Stdin, output, output, terminalToken)
	}
	log := slog.New(slog.NewJSONHandler(output, nil))
	flags := flag.NewFlagSet("dscregsyncd", flag.ContinueOnError)
	// flag errors echo user input; only help output is safe to print verbatim.
	flags.SetOutput(io.Discard)
	path := flags.String("config", config.DefaultPath(), "path to the service JSON configuration")
	printVersion := flags.Bool("version", false, "print the build version and exit")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(output, "Usage: dscregsyncd [-config path] [-version]\n       dscregsyncd register <machine-name> --server <registry-url> [--config path] [--token-stdin]\n\nregister provisions a new machine using a registration token (no service installation or startup).")
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
	if *printVersion {
		if _, err := fmt.Fprintf(output, "dscregsyncd %s\n", version); err != nil {
			return 1
		}
		return 0
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
	publisher := publish.New(cfg.ConfigDir, cfg.ResultsDir, log)
	log.Info("daemon started", "mode", "managed-sync", "committed_etag_present", etag != "")
	ready()
	resultsStopped := make(chan struct{})
	go func() {
		defer close(resultsStopped)
		results.New(publisher, client, cfg.ResultsDir, log).Run(ctx)
	}()
	poll(ctx, client, publisher, etag, time.Duration(cfg.PollFallbackSeconds)*time.Second, log, wait)
	<-resultsStopped
	log.Info("daemon stopped")
	return 0
}

func poll(ctx context.Context, client *registry.Client, publisher *publish.Publisher, etag string, fallback time.Duration,
	log *slog.Logger, sleep func(context.Context, time.Duration) error,
) {
	schedule := registry.Schedule{Fallback: fallback}
	for ctx.Err() == nil {
		response, err := client.Fetch(ctx, etag)
		if ctx.Err() != nil {
			return
		}
		if err == nil && response.Collection != nil {
			err = publisher.Apply(ctx, response.Collection, response.ETag)
			if err == nil {
				etag = response.ETag
			}
		}
		if ctx.Err() != nil {
			return
		}
		delay := schedule.Next(response.Header, err != nil, time.Now(), rand.Float64())
		switch {
		case err != nil:
			log.Warn("desired-state synchronization failed", "status", response.StatusCode, "error", err, "retry_in", delay)
		case response.Collection == nil:
			log.Info("desired state unchanged", "status", response.StatusCode, "next_poll_in", delay)
		default:
			log.Info("desired-state collection committed",
				"status", response.StatusCode, "configuration_count", len(response.Collection.Configurations),
				"next_poll_in", delay)
		}
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
