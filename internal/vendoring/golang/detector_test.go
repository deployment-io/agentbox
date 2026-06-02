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

func TestFinalizeWritesGoWork(t *testing.T) {
	d := &detector{}
	work := t.TempDir()
	a, b := filepath.Join(work, "0-kit"), filepath.Join(work, "1-app")
	for _, dir := range []string{a, b} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Finalize(work, []string{a, b}); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(work, "go.work"))
	if err != nil {
		t.Fatalf("go.work not written: %v", err)
	}
	for _, want := range []string{"go " + goWorkspaceVersion, "use (", "./0-kit", "./1-app"} {
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
