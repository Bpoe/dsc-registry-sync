package publish

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry-sync/internal/registry"
	"github.com/Bpoe/dsc-registry-sync/internal/testutil"
)

func TestSnapshotRevisionAndAbsence(t *testing.T) {
	p := newPublisher(t)
	ctx := context.Background()
	if _, err := p.SnapshotResult(ctx, "docker"); !errors.Is(err, ErrNoResult) {
		t.Fatal("missing directory was not absent")
	}
	apply(t, p, collection(revision1, nil), `"one"`)
	data := "{ \"schemaVersion\": 1, \"private\": \"<>&\" }\n"
	put(t, p.resultsDir, "docker.json.result.json", data)
	report, err := p.SnapshotResult(ctx, "docker")
	want, _ := registry.NewReport("docker", revision1, []byte(data))
	if err != nil || report.ID() != want.ID() || report.Revision() != revision1 {
		t.Fatalf("snapshot did not preserve bytes/revision: %v", err)
	}
	apply(t, p, collection(revision2, nil), `"two"`)
	if report.Revision() != revision1 || report.ID() != want.ID() {
		t.Fatal("captured historical snapshot was relabeled")
	}
	if _, err := p.SnapshotResult(ctx, "docker"); !errors.Is(err, ErrNoResult) {
		t.Fatal("new revision inherited old result")
	}
	put(t, p.resultsDir, "docker.json.result.json", data)
	for _, revision := range []string{"", "not-a-revision", revision2 + "\nextra"} {
		put(t, p.configDir, ".docker.revision", revision)
		if _, err := p.SnapshotResult(ctx, "docker"); !errors.Is(err, registry.ErrInvalidReport) {
			t.Fatal("unknown revision was accepted")
		}
	}
	if err := os.Remove(filepath.Join(p.configDir, "dscregsync", ".docker.revision")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.SnapshotResult(ctx, "docker"); !errors.Is(err, registry.ErrInvalidReport) {
		t.Fatal("missing revision was accepted")
	}
}

func TestSnapshotWaitsForPublicationAndCancels(t *testing.T) {
	p := newPublisher(t)
	apply(t, p, collection(revision1, nil), `"one"`)
	put(t, p.resultsDir, "docker.json.result.json", "{}")
	blocked, release := make(chan struct{}), make(chan struct{})
	p.after = func(name, step string) error {
		if step == "publish-revision" {
			close(blocked)
			<-release
		}
		return nil
	}
	finished := make(chan error, 1)
	go func() { finished <- p.Apply(context.Background(), collection(revision2, nil), `"two"`) }()
	<-blocked
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	_, err := p.SnapshotResult(ctx, "docker")
	cancel()
	close(release)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("snapshot did not use publication gate")
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if _, err := p.SnapshotResult(context.Background(), "docker"); !errors.Is(err, ErrNoResult) {
		t.Fatal("snapshot read stale result after publication")
	}
}

func TestSnapshotConfinedRegularFiles(t *testing.T) {
	for _, target := range []string{"result", "revision", "directory"} {
		t.Run(target, func(t *testing.T) {
			p := newPublisher(t)
			apply(t, p, collection(revision1, nil), `"one"`)
			put(t, p.resultsDir, "docker.json.result.json", "{}")
			outside := filepath.Join(t.TempDir(), "private")
			if err := os.WriteFile(outside, []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(p.resultsDir, "dscregsync", "docker.json.result.json")
			if target == "revision" {
				path = filepath.Join(p.configDir, "dscregsync", ".docker.revision")
			} else if target == "directory" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				path = filepath.Dir(path)
				outside = filepath.Dir(outside)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, path); err != nil {
				t.Skipf("symlink privilege unavailable: %v", err)
			}
			if _, err := p.SnapshotResult(context.Background(), "docker"); err == nil || strings.Contains(err.Error(), outside) {
				t.Fatal("followed link or disclosed its target")
			}
		})
	}
	p := newPublisher(t)
	for _, name := range []string{"../outside", "con", "docker.json/other", "UPPER", "docker.parameters"} {
		if _, err := p.SnapshotResult(context.Background(), name); err == nil {
			t.Fatal("unsafe snapshot name accepted")
		}
	}
}

func TestSnapshotValidationAndScan(t *testing.T) {
	p := newPublisher(t)
	apply(t, p, collection(revision1, nil), `"one"`)
	for _, data := range []string{`{"unfinished":`, "[]", "null", `{"x":1,"x":2}`, strings.Repeat(" ", registry.MaxResultBytes+1)} {
		put(t, p.resultsDir, "docker.json.result.json", data)
		if _, err := p.SnapshotResult(context.Background(), "docker"); !errors.Is(err, registry.ErrInvalidReport) {
			t.Fatal("invalid/oversize result was accepted")
		}
	}
	put(t, p.resultsDir, ".dscd-temp", "{}")
	put(t, p.resultsDir, "unrelated.txt", "private")
	names, err := p.ResultNames()
	if err != nil || len(names) != 1 || names[0] != "docker" {
		t.Fatal("scan included non-destination files")
	}
	put(t, p.resultsDir, "UPPER.json.result.json", "{}")
	if _, err := p.ResultNames(); err == nil {
		t.Fatal("scan accepted unsafe name")
	}
}

func TestStableFileChecksReplacementAndInPlaceChange(t *testing.T) {
	p := newPublisher(t)
	put(t, p.resultsDir, "docker.json.result.json", "{}")
	dir, err := openDirectory(p.resultsDir, false)
	if err != nil {
		t.Fatal(err)
	}

	defer dir.close()
	path := filepath.Join(p.resultsDir, "dscregsync", "docker.json.result.json")
	opened, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !stableFile(opened, opened, opened, 2) || stableFile(opened, opened, opened, 1) ||
		stableFile(opened, opened, nil, 2) {
		t.Fatal("stable identity/size check failed")
	}
	put(t, p.resultsDir, "replacement", "{}")
	if err := testutil.ReplaceResult(filepath.Join(p.resultsDir, "dscregsync", "replacement"), path); err != nil {
		t.Fatal(err)
	}
	replaced, err := os.Stat(path)
	if err != nil || stableFile(opened, opened, replaced, 2) {
		t.Fatal("replacement not detected")
	}
	put(t, p.resultsDir, "docker.json.result.json", "{ }")
	changed, err := os.Stat(path)
	if err != nil || stableFile(replaced, changed, changed, 2) {
		t.Fatal("in-place write not detected")
	}
}

func TestConcurrentAtomicResultSnapshots(t *testing.T) {
	p := newPublisher(t)
	apply(t, p, collection(revision1, nil), `"one"`)
	data := []string{`{"payload":"` + strings.Repeat("a", 64<<10) + `"}`,
		`{"payload":"` + strings.Repeat("b", 64<<10) + `"}`}
	allowed := make(map[string]bool)
	for _, value := range data {
		report, _ := registry.NewReport("docker", revision1, []byte(value))
		allowed[report.ID()] = true
	}
	put(t, p.resultsDir, "docker.json.result.json", data[0])
	done := make(chan error, 1)
	go func() {
		dir := filepath.Join(p.resultsDir, "dscregsync")
		for i := range 50 {
			temp := filepath.Join(dir, ".dscd-temp")
			if err := os.WriteFile(temp, []byte(data[i%2]), 0600); err != nil {
				done <- err
				return
			}
			if err := testutil.ReplaceResult(temp, filepath.Join(dir, "docker.json.result.json")); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for range 50 {
		report, err := p.SnapshotResult(context.Background(), "docker")
		if err == nil && !allowed[report.ID()] {
			t.Error("snapshot uploaded mixed/partial bytes")
		} else if err != nil && !errors.Is(err, errChanged) {
			t.Errorf("unexpected snapshot failure: %v", err)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
