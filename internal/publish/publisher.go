// Package publish owns Registry inputs and their committed revision state.
package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/Bpoe/dsc-registry-sync/internal/registry"
)

type unitLock struct {
	gate  chan struct{}
	users int
}

type Publisher struct {
	configDir, resultsDir string
	log                   *slog.Logger
	collections           chan struct{}
	mu                    sync.Mutex
	units                 map[string]*unitLock
	// Tests interrupt after individual filesystem mutations, never before them.
	after func(name, step string) error
}

// New does not touch disk: errors, offline operation and 304s remain read-only.
// One daemon must exclusively own the managed input subtree.
func New(configDir, resultsDir string, log *slog.Logger) *Publisher {
	return &Publisher{
		configDir: configDir, resultsDir: resultsDir, log: log,
		collections: make(chan struct{}, 1), units: make(map[string]*unitLock),
	}
}

// WithUnit serializes publication with result snapshot reads. Do not
// perform network I/O in action, or call Apply/WithUnit recursively from it.
func (p *Publisher) WithUnit(ctx context.Context, name string, action func() error) error {
	if !registry.ValidName(name) {
		return errors.New("invalid managed configuration name")
	}
	p.mu.Lock()
	lock := p.units[name]
	if lock == nil {
		lock = &unitLock{gate: make(chan struct{}, 1)}
		p.units[name] = lock
	}
	lock.users++
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		lock.users--
		if lock.users == 0 {
			delete(p.units, name)
		}
		p.mu.Unlock()
	}()
	select {
	case lock.gate <- struct{}{}:
		defer func() { <-lock.gate }()
		if err := ctx.Err(); err != nil {
			return err
		}
		return action()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Apply commits the validator only after the entire authoritative 200 succeeds.
// Partial changes are deliberately not rolled back; the old validator requests
// the collection again, and missing templates force incomplete units to retry.
func (p *Publisher) Apply(ctx context.Context, collection *registry.Collection, etag string) (err error) {
	if err := collection.Validate(); err != nil {
		return err
	}
	if !registry.ValidETag(etag) {
		return errors.New("invalid collection validator")
	}
	select {
	case p.collections <- struct{}{}:
		defer func() { <-p.collections }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Underlying filesystem errors can contain configured paths or link targets.
	// Preserve errors.Is without exposing those strings in routine logs.
	defer func() {
		if err != nil {
			err = &failure{operation: "apply managed collection", cause: err}
		}
	}()
	dir, err := openDirectory(p.configDir, true)
	if err != nil {
		return err
	}
	defer dir.close()
	if err := dir.cleanTemps(ctx); err != nil {
		return err
	}
	names, err := p.localNames(dir)
	if err != nil {
		return err
	}
	for _, unit := range collection.Configurations {
		delete(names, unit.Name)
		if err := p.WithUnit(ctx, unit.Name, func() error {
			return p.install(ctx, dir, unit)
		}); err != nil {
			p.log.Warn("managed unit publication failed", "unit", unit.Name)
			return err
		}
	}
	removed := make([]string, 0, len(names))
	for name := range names {
		removed = append(removed, name)
	}
	slices.Sort(removed)
	for _, name := range removed {
		if err := p.WithUnit(ctx, name, func() error {
			return p.remove(ctx, dir, name)
		}); err != nil {
			p.log.Warn("managed unit removal failed", "unit", name)
			return err
		}
	}
	// Flush all input directory changes before exposing the new validator.
	if err := syncDirectory(dir.root); err != nil {
		return err
	}
	temp, err := dir.stage([]byte(etag))
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			err = errors.Join(err, dir.removeTemp(temp))
		}
	}()
	if err := p.checkpoint(ctx, "", "stage-etag"); err != nil {
		return err
	}
	// This rename is the commit point; no fallible work follows a successful
	// commit. A lost validator after power failure only causes another full GET.
	err = dir.replace(ctx, temp, "machine.etag")
	committed = err == nil
	return err
}

func (p *Publisher) checkpoint(ctx context.Context, name, step string) error {
	if p.after != nil {
		if err := p.after(name, step); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (p *Publisher) localNames(dir *directory) (map[string]bool, error) {
	names := make(map[string]bool)
	entries, err := dir.entries()
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		file := entry.Name()
		if file == "machine.etag" {
			if _, err := dir.regular(file); err != nil {
				return nil, err
			}
			continue
		}
		var name string
		switch {
		case strings.HasSuffix(file, ".parameters.json"):
			name = strings.TrimSuffix(file, ".parameters.json")
		case strings.HasSuffix(file, ".json"):
			name = strings.TrimSuffix(file, ".json")
		case strings.HasPrefix(file, ".") && strings.HasSuffix(file, ".revision"):
			name = strings.TrimSuffix(strings.TrimPrefix(file, "."), ".revision")
		default:
			return nil, errors.New("unexpected entry in exclusively owned input directory")
		}
		if !registry.ValidName(name) {
			return nil, errors.New("unsafe local managed name")
		}
		if _, err := dir.regular(file); err != nil {
			return nil, err
		}
		names[name] = true
	}
	results, err := openDirectory(p.resultsDir, false)
	if err != nil || results == nil {
		return names, err
	}
	defer results.close()
	entries, err = results.entries()
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if name, ok := strings.CutSuffix(entry.Name(), ".json.result.json"); ok {
			if !registry.ValidName(name) {
				return nil, errors.New("unsafe local result name")
			}
			if _, err := results.regular(entry.Name()); err != nil {
				return nil, err
			}
			names[name] = true
		}
	}
	return names, nil
}

func (p *Publisher) install(ctx context.Context, dir *directory, unit registry.Configuration) (err error) {
	hasParameters := len(unit.Parameters) != 0 && !bytes.Equal(bytes.TrimSpace(unit.Parameters), []byte("null"))
	current, err := dir.current(unit.Name, unit.Revision, hasParameters)
	if err != nil || current {
		return err
	}
	temps := make([]string, 0, 3)
	defer func() {
		for _, temp := range temps {
			err = errors.Join(err, dir.removeTemp(temp))
		}
	}()
	stage := func(data []byte, step string) (string, error) {
		temp, err := dir.stage(data)
		if err != nil {
			return "", err
		}
		temps = append(temps, temp)
		return temp, p.checkpoint(ctx, unit.Name, step)
	}
	template, err := document(unit.Template)
	if err != nil {
		return err
	}
	configTemp, err := stage(template, "stage-config")
	if err != nil {
		return err
	}
	var parametersTemp string
	if hasParameters {
		parameters, err := document(unit.Parameters)
		if err != nil {
			return err
		}
		parametersTemp, err = stage(parameters, "stage-parameters")
		if err != nil {
			return err
		}
	}
	revisionTemp, err := stage([]byte(unit.Revision), "stage-revision")
	if err != nil {
		return err
	}
	if err := dir.removeConfiguration(ctx, unit.Name+".json"); err != nil {
		return err
	}
	if err := p.checkpoint(ctx, unit.Name, "remove-config"); err != nil {
		return err
	}
	if err := dir.remove(ctx, unit.Name+".parameters.json"); err != nil {
		return err
	}
	if err := p.checkpoint(ctx, unit.Name, "remove-parameters"); err != nil {
		return err
	}
	if err := p.removeResult(ctx, unit.Name); err != nil {
		return err
	}
	if err := p.checkpoint(ctx, unit.Name, "remove-result"); err != nil {
		return err
	}
	if hasParameters {
		if err := dir.replace(ctx, parametersTemp, unit.Name+".parameters.json"); err != nil {
			return err
		}
		if err := p.checkpoint(ctx, unit.Name, "publish-parameters"); err != nil {
			return err
		}
	}
	if err := dir.replace(ctx, revisionTemp, "."+unit.Name+".revision"); err != nil {
		return err
	}
	if err := p.checkpoint(ctx, unit.Name, "publish-revision"); err != nil {
		return err
	}
	if err := dir.replace(ctx, configTemp, unit.Name+".json"); err != nil {
		return err
	}
	if err := p.checkpoint(ctx, unit.Name, "publish-config"); err != nil {
		return err
	}
	p.log.Info("managed unit published", "unit", unit.Name)
	return nil
}

func (p *Publisher) remove(ctx context.Context, dir *directory, name string) error {
	if err := dir.removeConfiguration(ctx, name+".json"); err != nil {
		return err
	}
	if err := p.checkpoint(ctx, name, "remove-config"); err != nil {
		return err
	}
	if err := dir.remove(ctx, name+".parameters.json"); err != nil {
		return err
	}
	if err := p.checkpoint(ctx, name, "remove-parameters"); err != nil {
		return err
	}
	if err := p.removeResult(ctx, name); err != nil {
		return err
	}
	if err := p.checkpoint(ctx, name, "remove-result"); err != nil {
		return err
	}
	if err := dir.remove(ctx, "."+name+".revision"); err != nil {
		return err
	}
	if err := p.checkpoint(ctx, name, "remove-revision"); err != nil {
		return err
	}
	p.log.Info("managed unit removed", "unit", name)
	return nil
}

func (p *Publisher) removeResult(ctx context.Context, name string) error {
	// dscd can create this directory while we wait for its configuration lock.
	// Open it AFTER removing the protected old configuration, not at poll start.
	dir, err := openDirectory(p.resultsDir, false)
	if err != nil || dir == nil {
		return err
	}
	defer dir.close()
	if err := dir.remove(ctx, name+".json.result.json"); err != nil {
		return err
	}
	return syncDirectory(dir.root)
}

func document(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var data bytes.Buffer
	encoder := json.NewEncoder(&data)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(data.Bytes(), []byte{'\n'}), nil
}

type failure struct {
	operation string
	cause     error
}

func (e *failure) Error() string {
	return fmt.Sprintf("%s failed (check managed directory permissions, entries and file protection)", e.operation)
}
func (e *failure) Unwrap() error { return e.cause }
