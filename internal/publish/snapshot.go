package publish

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/Bpoe/dsc-registry-sync/internal/registry"
)

var (
	ErrNoResult = errors.New("no current local result")
	errChanged  = errors.New("managed file changed during snapshot")
)

// ResultNames scans only the confined managed directory; watcher filenames are
// hints, never authority to open arbitrary paths.
func (p *Publisher) ResultNames() (names []string, err error) {
	defer func() {
		if err != nil {
			err = &failure{operation: "scan managed results", cause: err}
		}
	}()
	dir, err := openDirectory(p.resultsDir, false)
	if err != nil || dir == nil {
		return nil, err
	}
	defer dir.close()
	entries, err := dir.entries()
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ".json.result.json")
		if !ok {
			continue
		}
		if !registry.ValidName(name) {
			return nil, errors.New("unsafe result name")
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names, nil
}

// SnapshotResult pairs exact result bytes with the installed revision under
// the SAME per-unit gate as publication. HTTP must happen after this returns.
func (p *Publisher) SnapshotResult(ctx context.Context, name string) (report *registry.Report, err error) {
	err = p.WithUnit(ctx, name, func() error {
		results, err := openDirectory(p.resultsDir, false)
		if err != nil {
			return err
		}
		if results == nil {
			return ErrNoResult
		}
		defer results.close()
		data, err := results.readStable(name+".json.result.json", registry.MaxResultBytes)
		if err != nil {
			return err
		}
		inputs, err := openDirectory(p.configDir, false)
		if err != nil {
			return err
		}
		if inputs == nil {
			return registry.ErrInvalidReport
		}
		defer inputs.close()
		template, err := inputs.regular(name + ".json")
		if err != nil {
			return err
		}
		if template == nil {
			return registry.ErrInvalidReport
		}
		revision, err := inputs.readStable("."+name+".revision", 38)
		if errors.Is(err, ErrNoResult) {
			return registry.ErrInvalidReport
		}
		if err != nil {
			return err
		}
		value := strings.TrimSuffix(strings.TrimSuffix(string(revision), "\n"), "\r")
		if err := ctx.Err(); err != nil {
			return err
		}
		report, err = registry.NewReport(name, value, data)
		return err
	})
	if err != nil {
		err = &failure{operation: "snapshot managed result", cause: err}
	}
	return report, err
}

func (d *directory) readStable(name string, limit int64) (data []byte, err error) {
	before, err := d.regular(name)
	if err != nil {
		return nil, err
	}
	if before == nil {
		return nil, ErrNoResult
	}
	file, err := openRegular(d.root, name)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, errChanged
	}
	if opened.Size() > limit {
		return nil, registry.ErrInvalidReport
	}
	data, err = io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	current, err := d.regular(name)
	if err != nil {
		return nil, err
	}
	if !stableFile(opened, after, current, len(data)) {
		return nil, errChanged
	}
	if len(data) > int(limit) {
		return nil, registry.ErrInvalidReport
	}
	return data, nil
}

func stableFile(opened, after, current os.FileInfo, size int) bool {
	return current != nil && os.SameFile(opened, after) && os.SameFile(opened, current) &&
		opened.Size() == int64(size) && after.Size() == opened.Size() && current.Size() == opened.Size() &&
		opened.ModTime().Equal(after.ModTime()) && opened.ModTime().Equal(current.ModTime())
}
