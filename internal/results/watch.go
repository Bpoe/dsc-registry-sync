package results

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Bpoe/dsc-registry-sync/internal/registry"
	"github.com/fsnotify/fsnotify"
)

func (s *Service) watch(ctx context.Context) {
	delay := s.recovery
	for ctx.Err() == nil {
		start := time.Now()
		err := s.watchOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			// A directory was created/moved: reattach before scanning the new tree.
			continue
		}
		s.log.Warn("result watcher unavailable; reattaching with recovery scan", "retry_in", delay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if time.Since(start) > time.Minute {
			delay = s.recovery
		} else {
			delay = min(delay*2, 30*time.Second)
		}
	}
}

func (s *Service) watchOnce(ctx context.Context) (err error) {
	watcher, err := s.newWatcher()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, watcher.Close()) }()
	target := filepath.Join(s.root, "dscregsync")
	paths := directoryChain(target)
	// Subscribe to each existing ancestor before inspecting its child. This
	// covers missing roots, fast mkdir+publish, and replacement of watched dirs.
	for _, path := range paths {
		var info os.FileInfo
		if path == target {
			info, err = os.Lstat(path)
		} else {
			info, err = os.Stat(path) // administrator-provisioned ancestors are trusted
		}
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("result watch path is not a directory")
		}
		if err := watcher.Add(path); err != nil {
			return err
		}
		// A path-based subscription is only a hint. Reject substitutions during
		// setup; all content reads still use the publisher's confined handles.
		var current os.FileInfo
		if path == target {
			current, err = os.Lstat(path)
		} else {
			current, err = os.Stat(path)
		}
		if err != nil {
			return err
		}
		if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, current) {
			return errors.New("result watch directory changed")
		}
	}
	names, err := s.publisher.ResultNames()
	if err != nil {
		return err
	}
	s.changes.reconcile(names)
	s.log.Info("result watcher attached")
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-watcher.Events:
			if !ok {
				return errors.New("result watcher closed")
			}
			path := filepath.Clean(event.Name)
			if slices.ContainsFunc(paths, func(dir string) bool { return samePath(dir, path) }) &&
				event.Has(fsnotify.Create|fsnotify.Remove|fsnotify.Rename|fsnotify.Chmod) {
				return nil
			}
			if !samePath(filepath.Dir(path), target) || !event.Has(fsnotify.Create|fsnotify.Rename|fsnotify.Write|fsnotify.Remove) {
				continue
			}
			name, ok := strings.CutSuffix(filepath.Base(path), ".json.result.json")
			if !ok {
				continue
			}
			if !registry.ValidName(name) {
				s.log.Warn("ignoring unsafe result notification name")
				continue
			}
			s.changes.notify(name)
		case _, ok := <-watcher.Errors:
			if !ok {
				return errors.New("result watcher closed")
			}
			// Includes overflow: lost notifications are repaired by the next scan.
			return errors.New("result watcher failed")
		}
	}
}

func directoryChain(target string) []string {
	var paths []string
	for {
		paths = append(paths, target)
		parent := filepath.Dir(target)
		if parent == target {
			break
		}
		target = parent
	}
	slices.Reverse(paths)
	return paths
}

func samePath(a, b string) bool {
	// Rel uses platform path comparison, including Windows case insensitivity.
	relative, err := filepath.Rel(a, b)
	return err == nil && relative == "."
}
