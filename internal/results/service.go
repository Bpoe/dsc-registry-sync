// Package results observes latest result publications, not execution history.
package results

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/Bpoe/dsc-registry-sync/internal/publish"
	"github.com/Bpoe/dsc-registry-sync/internal/registry"
	"github.com/fsnotify/fsnotify"
)

type Service struct {
	publisher  *publish.Publisher
	client     *registry.Client
	root       string
	log        *slog.Logger
	changes    changes
	debounce   time.Duration
	recovery   time.Duration
	newWatcher func() (*fsnotify.Watcher, error)
}

func New(publisher *publish.Publisher, client *registry.Client, resultsDir string, log *slog.Logger) *Service {
	return &Service{
		publisher: publisher, client: client, root: resultsDir, log: log,
		changes:  changes{wake: make(chan struct{}, 1), names: make(map[string]bool)},
		debounce: 100 * time.Millisecond, recovery: time.Second, newWatcher: fsnotify.NewWatcher,
	}
}

// Run joins the watcher before returning. Only names/IDs are retained between
// attempts; each retry reads the latest file, never a historical in-memory queue.
func (s *Service) Run(ctx context.Context) {
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		s.watch(ctx)
	}()
	defer func() { <-stopped }()
	states := make(map[string]*pending)
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for ctx.Err() == nil {
		names, scan, scanned := s.changes.take()
		if scanned {
			for _, name := range scan {
				names[name] = true
			}
			// Recheck disappeared destinations too, without letting a directory
			// replacement discard an outstanding Retry-After deadline.
			for name := range states {
				names[name] = true
			}
		}
		for name := range names {
			state := states[name]
			if state == nil {
				state = &pending{}
				states[name] = state
			}
			// Duplicate events must not shorten a failed attempt's Retry-After.
			if state.due.IsZero() {
				state.due = time.Now().Add(s.debounce)
			}
		}
		var name string
		var due time.Time
		for candidate, state := range states {
			if !state.due.IsZero() && (due.IsZero() || state.due.Before(due)) {
				name, due = candidate, state.due
			}
		}
		if !due.IsZero() && !time.Now().Before(due) {
			if s.attempt(ctx, name, states[name]) {
				delete(states, name)
			}
			continue
		}
		var tick <-chan time.Time
		if !due.IsZero() {
			timer.Reset(time.Until(due))
			tick = timer.C
		}
		select {
		case <-ctx.Done():
		case <-s.changes.wake:
		case <-tick:
		}
		timer.Stop()
	}
}

type pending struct {
	id       string
	done     bool
	due      time.Time
	schedule registry.Schedule
}

// attempt returns true when the destination disappeared and its state can go.
func (s *Service) attempt(ctx context.Context, name string, state *pending) bool {
	state.due = time.Time{}
	report, err := s.publisher.SnapshotResult(ctx, name)
	if ctx.Err() != nil {
		return false
	}
	if errors.Is(err, publish.ErrNoResult) {
		return true
	}
	if err != nil {
		if !errors.Is(err, registry.ErrInvalidReport) {
			state.due = time.Now().Add(state.schedule.Next(nil, true, time.Now(), rand.Float64()))
		}
		s.log.Warn("result snapshot unavailable", "unit", name, "retry_scheduled", !state.due.IsZero(), "error", err)
		return false
	}
	if report.ID() == state.id && state.done {
		return false
	}
	if report.ID() != state.id {
		state.id, state.done, state.schedule = report.ID(), false, registry.Schedule{}
	}
	response, err := s.client.Upload(ctx, report)
	if ctx.Err() != nil {
		return false
	}
	if err == nil {
		state.done = true
		s.log.Info("result uploaded", "unit", name, "status", response.StatusCode)
	} else if response.Permanent {
		state.done = true
		s.log.Error("result rejected; unchanged report will not be retried", "unit", name, "status", response.StatusCode)
	} else {
		delay := state.schedule.Next(response.Header, true, time.Now(), rand.Float64())
		state.due = time.Now().Add(delay)
		s.log.Warn("result upload failed", "unit", name, "status", response.StatusCode, "retry_in", delay, "error", err)
	}
	return false
}

// changes coalesces notification hints while HTTP is busy. It owns no payloads.
type changes struct {
	mu      sync.Mutex
	wake    chan struct{}
	names   map[string]bool
	scan    []string
	scanned bool
}

func (c *changes) notify(name string) {
	c.mu.Lock()
	c.names[name] = true
	c.mu.Unlock()
	c.signal()
}

func (c *changes) reconcile(names []string) {
	c.mu.Lock()
	c.scan, c.scanned = names, true
	c.mu.Unlock()
	c.signal()
}

func (c *changes) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *changes) take() (map[string]bool, []string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	names, scan, scanned := c.names, c.scan, c.scanned
	c.names, c.scan, c.scanned = make(map[string]bool), nil, false
	return names, scan, scanned
}
