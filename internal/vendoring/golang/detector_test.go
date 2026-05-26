package golang

import (
	"context"
	"os"
	"path/filepath"
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
}
