package golang

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectGoMod(t *testing.T) {
	d := &detector{}
	dir := t.TempDir()

	ok, err := d.Detect(dir)
	if err != nil {
		t.Fatalf("Detect (no go.mod): %v", err)
	}
	if ok {
		t.Error("Detect should be false when go.mod is absent")
	}

	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ok, err = d.Detect(dir)
	if err != nil {
		t.Fatalf("Detect (with go.mod): %v", err)
	}
	if !ok {
		t.Error("Detect should be true when go.mod is present")
	}
}

func TestMetadata(t *testing.T) {
	d := &detector{}
	if d.Name() != "go" {
		t.Errorf("Name() = %q, want \"go\"", d.Name())
	}
	if err := d.EnsureToolchain(context.Background()); err != nil {
		t.Errorf("EnsureToolchain() = %v, want nil (Go is baseline)", err)
	}
	if len(d.AllowedHosts()) == 0 {
		t.Error("AllowedHosts() should be non-empty")
	}
	if len(d.VerifyHosts()) == 0 {
		t.Error("VerifyHosts() should be non-empty")
	}
}

func TestEnv(t *testing.T) {
	d := &detector{}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module github.com/acme/svc\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// With a shared /cache mounted: every cache-like var moves onto the
	// disk-backed shelf. GOCACHE and GOTMPDIR matter just as much as
	// GOMODCACHE — /tmp tmpfs is only 512 MB, kit-scale builds fill it
	// and ENOSPC cascades into every shell command failing.
	want := map[string]bool{
		"GOMODCACHE=/cache/gomod":   true,
		"GOCACHE=/cache/gobuild":    true,
		"GOTMPDIR=/cache/gotmp":     true,
		"GOPRIVATE=github.com/acme": true,
	}
	for _, kv := range d.Env("/cache", []string{dir}) {
		delete(want, kv)
	}
	if len(want) != 0 {
		t.Errorf("Env(\"/cache\") missing entries: %v", want)
	}

	// No cacheDir → none of the redirects fire (let Go use its defaults).
	// Standalone dev/integration runs without a /cache mount stay usable.
	for _, kv := range d.Env("", []string{dir}) {
		for _, k := range []string{"GOMODCACHE=", "GOCACHE=", "GOTMPDIR="} {
			if strings.HasPrefix(kv, k) {
				t.Errorf("Env(\"\") should not set %s, got %q", k, kv)
			}
		}
	}
}

// TestEnvCreatesCacheSubdirs guards the v1.3.5 regression where
// /cache/gobuild and /cache/gotmp were referenced in env but never
// created — `go build` then aborts with a misleading mkdir error
// before doing useful work. Env must MkdirAll the subdirs it points at.
func TestEnvCreatesCacheSubdirs(t *testing.T) {
	d := &detector{}
	cache := t.TempDir()
	_ = d.Env(cache, nil)
	for _, sub := range []string{"gomod", "gobuild", "gotmp"} {
		path := filepath.Join(cache, sub)
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("Env should create %s: %v", path, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%s exists but is not a directory", path)
		}
	}
}

func TestFinalizeWritesGoWork(t *testing.T) {
	d := &detector{}
	work := t.TempDir()
	a, b := filepath.Join(work, "0-kit"), filepath.Join(work, "1-app")
	for _, dir := range []string{a, b} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The workspace's go line must be the highest module directive, not a
	// constant: a go.work older than any module fails every go command.
	if err := os.WriteFile(filepath.Join(a, "go.mod"), []byte("module kit\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, "go.mod"), []byte("module app\n\ngo 1.25.14\n\ntoolchain go1.25.14\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.Finalize(work, []string{a, b}); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(work, "go.work"))
	if err != nil {
		t.Fatalf("go.work not written: %v", err)
	}
	for _, want := range []string{"go 1.25.14\n", "use (", "./0-kit", "./1-app"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("go.work missing %q; got:\n%s", want, data)
		}
	}

	// <2 modules → no go.work.
	solo := t.TempDir()
	if err := d.Finalize(solo, []string{filepath.Join(solo, "0-only")}); err != nil {
		t.Fatalf("Finalize(1 repo): %v", err)
	}
	if _, err := os.Stat(filepath.Join(solo, "go.work")); !os.IsNotExist(err) {
		t.Error("go.work should not be written for <2 modules")
	}
}

func TestWorkspaceGoVersion(t *testing.T) {
	work := t.TempDir()
	mod := func(name, body string) string {
		dir := filepath.Join(work, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if body != "" {
			if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	cases := []struct {
		name string
		dirs []string
		want string
	}{
		{"no go.mod anywhere falls back to the floor", []string{mod("none", "")}, goWorkspaceFloor},
		{"go.mod without a go line falls back to the floor", []string{mod("old", "module old\n")}, goWorkspaceFloor},
		{"single module", []string{mod("one", "module one\n\ngo 1.25.0\n")}, "1.25.0"},
		{"highest wins, numerically not lexically", []string{
			mod("a", "module a\n\ngo 1.25.14\n"), mod("b", "module b\n\ngo 1.25.2\n"), mod("c", "module c\n\ngo 1.9\n"),
		}, "1.25.14"},
		{"a pre-release directive still orders by its numeric prefix", []string{
			mod("rc", "module rc\n\ngo 1.26rc1\n"), mod("d", "module d\n\ngo 1.25.14\n"),
		}, "1.26rc1"},
		{"the go line is matched at line start, not inside a require block", []string{
			mod("e", "module e\n\ngo 1.24\n\nrequire (\n\tgolang.org/x/tools v0.47.0 // go 1.99 in a comment\n)\n"),
		}, "1.24"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := workspaceGoVersion(tc.dirs); got != tc.want {
				t.Errorf("workspaceGoVersion = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCompareGoVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"1.25.14", "1.25.2", 1}, {"1.25", "1.25.0", 0}, {"1.24", "1.25", -1}, {"1.26rc1", "1.25.14", 1}, {"1.25.14", "1.25.14", 0},
	} {
		if got := compareGoVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("compare(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
