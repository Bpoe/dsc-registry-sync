package publish

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry-sync/internal/registry"
)

const revision1 = "11111111-1111-4111-8111-111111111111"
const revision2 = "22222222-2222-4222-8222-222222222222"

func newPublisher(t *testing.T) *Publisher {
	t.Helper()
	root := t.TempDir()
	return New(filepath.Join(root, "config.d"), filepath.Join(root, "results.d"),
		slog.New(slog.NewJSONHandler(io.Discard, nil)))
}

func collection(revision string, parameters []byte) *registry.Collection {
	return &registry.Collection{Configurations: []registry.Configuration{{
		Name: "docker", Revision: revision,
		Template:   []byte(`{ "resources": [], "large": 123456789012345678901234567890, "tiny": 1e-9999 }`),
		Parameters: parameters,
	}}}
}

func apply(t *testing.T, p *Publisher, c *registry.Collection, tag string) {
	t.Helper()
	if err := p.Apply(context.Background(), c, tag); err != nil {
		t.Fatal(err)
	}
}

func put(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, "dscregsync", name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func contents(t *testing.T, root, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "dscregsync", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func absent(t *testing.T, root, name string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(root, "dscregsync", name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s remains or is inaccessible: %v", name, err)
	}
}

func tagIs(t *testing.T, p *Publisher, tag string) {
	t.Helper()
	got, err := registry.ReadETag(p.configDir)
	if err != nil || got != tag {
		t.Fatalf("committed validator = %q, %v; want %q", got, err, tag)
	}
}

func files(t *testing.T, root string) map[string]string {
	t.Helper()
	result := make(map[string]string)
	entries, err := os.ReadDir(filepath.Join(root, "dscregsync"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		result[entry.Name()] = contents(t, root, entry.Name())
	}
	return result
}

func TestApplyLifecycle(t *testing.T) {
	p := newPublisher(t)
	// Sibling configurations/results are not part of the managed namespace.
	for _, root := range []string{p.configDir, p.resultsDir} {
		if err := os.MkdirAll(filepath.Join(root, "local"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "local", "docker.json"), []byte("private"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	c := collection(revision1, []byte(`{ "z": 1, "a": {} }`))
	apply(t, p, c, `W/"one"`)
	want := map[string]string{
		"docker.json":            `{"large":123456789012345678901234567890,"resources":[],"tiny":1e-9999}`,
		"docker.parameters.json": `{"a":{},"z":1}`,
		".docker.revision":       revision1,
		"machine.etag":           `W/"one"`,
	}
	if got := files(t, p.configDir); !reflect.DeepEqual(got, want) {
		t.Fatalf("published files: %v", got)
	}
	put(t, p.resultsDir, "docker.json.result.json", "old result")
	p.after = func(name, step string) error {
		if name != "" {
			t.Fatalf("unchanged unit republished at %s", step)
		}
		return nil
	}
	apply(t, p, c, `"same"`)
	if contents(t, p.resultsDir, "docker.json.result.json") != "old result" {
		t.Fatal("unchanged unit lost its result")
	}
	p.after = nil
	apply(t, p, collection(revision2, nil), `"two"`)
	absent(t, p.configDir, "docker.parameters.json")
	absent(t, p.resultsDir, "docker.json.result.json")
	if contents(t, p.configDir, ".docker.revision") != revision2 {
		t.Fatal("revision not replaced")
	}
	apply(t, p, collection(revision2, []byte(`{}`)), `"empty-parameters"`)
	if contents(t, p.configDir, "docker.parameters.json") != "{}" {
		t.Fatal("empty object was treated as no sidecar")
	}
	apply(t, p, collection(revision2, []byte(`null`)), `"null-parameters"`)
	absent(t, p.configDir, "docker.parameters.json")
	put(t, p.resultsDir, "docker.json.result.json", "completed result")
	apply(t, p, &registry.Collection{Configurations: []registry.Configuration{}}, `"empty"`)
	if got := files(t, p.configDir); !reflect.DeepEqual(got, map[string]string{"machine.etag": `"empty"`}) {
		t.Fatalf("authoritative removal left inputs: %v", got)
	}
	absent(t, p.resultsDir, "docker.json.result.json")
	for _, root := range []string{p.configDir, p.resultsDir} {
		data, err := os.ReadFile(filepath.Join(root, "local", "docker.json"))
		if err != nil || string(data) != "private" {
			t.Fatal("modified local sibling")
		}
	}
}

func TestMissingFilesAndSidecarPresenceForceRetry(t *testing.T) {
	for _, missing := range []string{"docker.json", "docker.parameters.json", ".docker.revision"} {
		t.Run(missing, func(t *testing.T) {
			p := newPublisher(t)
			c := collection(revision1, []byte(`{}`))
			apply(t, p, c, `"one"`)
			if err := os.Remove(filepath.Join(p.configDir, "dscregsync", missing)); err != nil {
				t.Fatal(err)
			}
			put(t, p.resultsDir, "docker.json.result.json", "old result")
			apply(t, p, c, `"repair"`)
			if len(files(t, p.configDir)) != 4 {
				t.Fatal("missing file not restored")
			}
			absent(t, p.resultsDir, "docker.json.result.json")
		})
	}
	p := newPublisher(t)
	c := collection(revision1, nil)
	apply(t, p, c, `"one"`)
	put(t, p.configDir, "docker.parameters.json", "{}")
	put(t, p.configDir, ".docker.revision", "corrupt")
	apply(t, p, c, `"repair"`)
	absent(t, p.configDir, "docker.parameters.json")
	if contents(t, p.configDir, ".docker.revision") != revision1 {
		t.Fatal("malformed revision was not repaired")
	}
}

func TestRetryKeepsAlreadyPublishedUnitsAndResults(t *testing.T) {
	p := newPublisher(t)
	apply(t, p, collection(revision1, nil), `"old"`)
	c := collection(revision2, nil)
	c.Configurations = append(c.Configurations, registry.Configuration{
		Name: "second", Revision: revision2, Template: []byte(`{}`),
	})
	p.after = func(name, step string) error {
		if name == "second" && step == "publish-revision" {
			return errors.New("second unit interrupted")
		}
		return nil
	}
	if err := p.Apply(context.Background(), c, `"new"`); err == nil {
		t.Fatal("expected incomplete collection")
	}
	tagIs(t, p, `"old"`)
	if contents(t, p.configDir, ".docker.revision") != revision2 {
		t.Fatal("completed first unit was rolled back")
	}
	put(t, p.resultsDir, "docker.json.result.json", "new revision result")
	p = New(p.configDir, p.resultsDir, p.log)
	p.after = func(name, step string) error {
		if name == "docker" {
			t.Fatal("retry unnecessarily republished completed unit")
		}
		return nil
	}
	apply(t, p, c, `"new"`)
	tagIs(t, p, `"new"`)
	if contents(t, p.resultsDir, "docker.json.result.json") != "new revision result" {
		t.Fatal("retry removed a result belonging to the completed new revision")
	}
}

func TestInterruptionAndRestartAtEveryPublicationBoundary(t *testing.T) {
	updateSteps := []string{"stage-config", "stage-parameters", "stage-revision", "remove-config",
		"remove-parameters", "remove-result", "publish-parameters", "publish-revision", "publish-config", "stage-etag"}
	for _, initial := range []bool{false, true} {
		for _, step := range updateSteps {
			t.Run(map[bool]string{false: "add/", true: "update/"}[initial]+step, func(t *testing.T) {
				p := newPublisher(t)
				oldTag := ""
				if initial {
					apply(t, p, collection(revision1, []byte(`{"old":true}`)), `"old"`)
					put(t, p.resultsDir, "docker.json.result.json", "previous result")
					oldTag = `"old"`
				}
				c := collection(revision2, []byte(`{"new":true}`))
				c.Configurations = append(c.Configurations, registry.Configuration{
					Name: "second", Revision: revision2, Template: []byte(`{}`),
				})
				injected := errors.New("injected interruption")
				fired := false
				p.after = func(name, got string) error {
					if got == step && (name == "docker" || name == "") {
						fired = true
						return injected
					}
					return nil
				}
				if err := p.Apply(context.Background(), c, `"new"`); !errors.Is(err, injected) || !fired {
					t.Fatalf("injection did not interrupt: %v", err)
				}
				tagIs(t, p, oldTag)
				restarted := New(p.configDir, p.resultsDir, p.log)
				apply(t, restarted, c, `"new"`)
				tagIs(t, p, `"new"`)
				if contents(t, p.configDir, ".docker.revision") != revision2 ||
					contents(t, p.configDir, "docker.parameters.json") != `{"new":true}` ||
					contents(t, p.configDir, "second.json") != "{}" || len(files(t, p.configDir)) != 6 {
					t.Fatal("retry did not complete exact collection")
				}
				absent(t, p.resultsDir, "docker.json.result.json")
			})
		}
	}
}

func TestInterruptedRemovalAndOrphanRecovery(t *testing.T) {
	for _, step := range []string{"remove-config", "remove-parameters", "remove-result", "remove-revision", "stage-etag"} {
		t.Run(step, func(t *testing.T) {
			p := newPublisher(t)
			apply(t, p, collection(revision1, []byte(`{}`)), `"old"`)
			put(t, p.resultsDir, "docker.json.result.json", "{}")
			p.after = func(name, got string) error {
				if got == step {
					return errors.New("interrupted")
				}
				return nil
			}
			empty := &registry.Collection{Configurations: []registry.Configuration{}}
			if err := p.Apply(context.Background(), empty, `"empty"`); err == nil {
				t.Fatal("expected interruption")
			}
			tagIs(t, p, `"old"`)
			p = New(p.configDir, p.resultsDir, p.log)
			apply(t, p, empty, `"empty"`)
			if len(files(t, p.configDir)) != 1 {
				t.Fatal("orphan inputs remain")
			}
			absent(t, p.resultsDir, "docker.json.result.json")
		})
	}
	p := newPublisher(t)
	put(t, p.configDir, "sidecar.parameters.json", "{}")
	put(t, p.configDir, ".revision-only.revision", revision1)
	put(t, p.configDir, tempPrefix+rand.Text()+".tmp", "abandoned staging")
	put(t, p.resultsDir, "result-only.json.result.json", "{}")
	put(t, p.resultsDir, ".dscd-in-progress", "not owned by sync")
	apply(t, p, &registry.Collection{Configurations: []registry.Configuration{}}, `"empty"`)
	if len(files(t, p.configDir)) != 1 || len(files(t, p.resultsDir)) != 1 {
		t.Fatal("orphan cleanup incorrect")
	}
}

func TestPublicationOrderAndLateResultDirectory(t *testing.T) {
	p := newPublisher(t)
	apply(t, p, collection(revision1, []byte(`{"old":true}`)), `"old"`)
	var steps []string
	p.after = func(name, step string) error {
		steps = append(steps, step)
		if step == "remove-config" {
			// Mimic the previously protected reconciliation publishing its
			// result into a directory absent when collection application began.
			put(t, p.resultsDir, "docker.json.result.json", "late old result")
		}
		switch step {
		case "remove-config", "remove-parameters", "remove-result", "publish-parameters", "publish-revision":
			absent(t, p.configDir, "docker.json")
		case "publish-config":
			absent(t, p.resultsDir, "docker.json.result.json")
			if contents(t, p.configDir, ".docker.revision") != revision2 ||
				contents(t, p.configDir, "docker.parameters.json") != `{"new":true}` {
				t.Fatal("template exposed before matching sidecars")
			}
		}
		tagIs(t, p, `"old"`)
		return nil
	}
	apply(t, p, collection(revision2, []byte(`{"new":true}`)), `"new"`)
	want := []string{"stage-config", "stage-parameters", "stage-revision", "remove-config",
		"remove-parameters", "remove-result", "publish-parameters", "publish-revision", "publish-config", "stage-etag"}
	if !reflect.DeepEqual(steps, want) {
		t.Fatalf("publication order: %v", steps)
	}
}

func TestFullCollectionValidationPrecedesMutation(t *testing.T) {
	for _, bad := range []registry.Configuration{
		{Name: "../outside", Revision: revision1, Template: []byte(`{}`)},
		{Name: "con", Revision: revision1, Template: []byte(`{}`)},
		{Name: "safe", Revision: "not-a-revision", Template: []byte(`{}`)},
		{Name: "safe", Revision: revision1, Template: []byte(`[]`)},
		{Name: "safe", Revision: revision1, Template: []byte(`{"a":1,"a":2}`)},
		{Name: "docker", Revision: revision1, Template: []byte(`{}`)},
	} {
		p := newPublisher(t)
		c := collection(revision1, nil)
		c.Configurations = append(c.Configurations, bad)
		if err := p.Apply(context.Background(), c, `"new"`); err == nil {
			t.Fatal("accepted invalid collection")
		}
		if _, err := os.Stat(p.configDir); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("invalid response mutated filesystem")
		}
	}
	p := newPublisher(t)
	for _, c := range []*registry.Collection{nil, {}} {
		if err := p.Apply(context.Background(), c, `"new"`); err == nil {
			t.Fatal("missing collection accepted as authoritative empty")
		}
	}
	if err := p.Apply(context.Background(), collection(revision1, nil), "unquoted"); err == nil {
		t.Fatal("invalid ETag accepted")
	}
}

func TestOpaqueDocumentWithinWireLimit(t *testing.T) {
	p := newPublisher(t)
	c := collection(revision1, nil)
	raw := `{"opaque":"` + strings.Repeat("<>&", 100000) + `"}`
	c.Configurations[0].Template = []byte(raw)
	apply(t, p, c, `"large"`)
	if got := contents(t, p.configDir, "docker.json"); got != raw {
		t.Fatal("opaque document expanded or changed during validation/publication")
	}
}

func TestNonregularFilesAndSymlinksAreRejected(t *testing.T) {
	for _, name := range []string{"docker.json", "docker.parameters.json", ".docker.revision", "machine.etag"} {
		t.Run("directory/"+name, func(t *testing.T) {
			p := newPublisher(t)
			path := filepath.Join(p.configDir, "dscregsync", name)
			if err := os.MkdirAll(path, 0700); err != nil {
				t.Fatal(err)
			}
			if err := p.Apply(context.Background(), collection(revision1, nil), `"new"`); err == nil {
				t.Fatal("accepted directory in place of managed file")
			}
		})
	}
	for _, target := range []string{"input-directory", "result-directory", "docker.json",
		"docker.parameters.json", ".docker.revision", "machine.etag", "result"} {
		t.Run("symlink/"+target, func(t *testing.T) {
			p := newPublisher(t)
			outside := t.TempDir()
			victim := filepath.Join(outside, "victim")
			if err := os.WriteFile(victim, []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(p.configDir, 0700); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(p.configDir, "dscregsync")
			destination := outside
			switch target {
			case "input-directory":
			case "result-directory":
				if err := os.MkdirAll(p.resultsDir, 0700); err != nil {
					t.Fatal(err)
				}
				link = filepath.Join(p.resultsDir, "dscregsync")
			case "result":
				if err := os.MkdirAll(filepath.Join(p.resultsDir, "dscregsync"), 0700); err != nil {
					t.Fatal(err)
				}
				link = filepath.Join(p.resultsDir, "dscregsync", "docker.json.result.json")
				destination = victim
			default:
				if err := os.Mkdir(link, 0700); err != nil {
					t.Fatal(err)
				}
				link = filepath.Join(link, target)
				destination = victim
			}
			if err := os.Symlink(destination, link); err != nil {
				if runtime.GOOS == "windows" {
					t.Skip("symlink privilege unavailable:", err)
				}
				t.Fatal(err)
			}
			if err := p.Apply(context.Background(), collection(revision1, nil), `"new"`); err == nil {
				t.Fatal("followed managed symlink")
			}
			data, err := os.ReadFile(victim)
			if err != nil || string(data) != "untouched" {
				t.Fatal("modified symlink target")
			}
		})
	}
}

func TestDirectoryReplacementBeforePublication(t *testing.T) {
	p := newPublisher(t)
	apply(t, p, collection(revision1, nil), `"old"`)
	p.after = func(name, step string) error {
		if step != "stage-revision" {
			return nil
		}
		path := filepath.Join(p.configDir, "dscregsync")
		if err := os.Rename(path, filepath.Join(p.configDir, "displaced")); err != nil {
			return err
		}
		return os.Mkdir(path, 0700)
	}
	if err := p.Apply(context.Background(), collection(revision2, nil), `"new"`); err == nil {
		t.Fatal("continued writing after directory replacement")
	}
	entries, err := os.ReadDir(filepath.Join(p.configDir, "dscregsync"))
	if err != nil || len(entries) != 0 {
		t.Fatal("wrote into substituted directory")
	}
}

func TestWithUnitSerializationAndCancellation(t *testing.T) {
	p := newPublisher(t)
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		finished <- p.WithUnit(context.Background(), "docker", func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := p.WithUnit(ctx, "docker", func() error {
		t.Error("overlapped same unit")
		return nil
	}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("unit wait did not cancel")
	}
	if err := p.WithUnit(context.Background(), "different", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- p.Apply(context.Background(), collection(revision1, nil), `"new"`) }()
	select {
	case <-done:
		t.Fatal("Apply bypassed per-unit serialization")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(p.units) != 0 {
		t.Fatal("unused unit locks leaked")
	}
}

func TestPermissionsAndPrivateErrors(t *testing.T) {
	p := newPublisher(t)
	apply(t, p, collection(revision1, []byte(`{"secret":"never-log"}`)), `"private-tag"`)
	if runtime.GOOS == "linux" {
		for name := range files(t, p.configDir) {
			info, err := os.Stat(filepath.Join(p.configDir, "dscregsync", name))
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("managed file is not private")
			}
		}
		info, err := os.Stat(filepath.Join(p.configDir, "dscregsync"))
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatal("managed directory is not private")
		}
	}
	var logs bytes.Buffer
	p.log = slog.New(slog.NewJSONHandler(&logs, nil))
	p.after = func(name, step string) error { return errors.New("secret path and data") }
	err := p.Apply(context.Background(), collection(revision2, nil), `"new-private-tag"`)
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(logs.String(), "secret") {
		t.Fatal("underlying failure leaked sensitive details")
	}
}
