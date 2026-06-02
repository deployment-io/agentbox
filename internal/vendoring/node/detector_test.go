package node

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDetectPackageJSON(t *testing.T) {
	d := &detector{}
	dir := t.TempDir()

	ok, err := d.Detect(dir)
	if err != nil {
		t.Fatalf("Detect (no package.json): %v", err)
	}
	if ok {
		t.Error("Detect should be false when package.json is absent")
	}

	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ok, err = d.Detect(dir)
	if err != nil {
		t.Fatalf("Detect (with package.json): %v", err)
	}
	if !ok {
		t.Error("Detect should be true when package.json is present")
	}
}

func TestInstallCommand(t *testing.T) {
	cases := []struct {
		name      string
		lockfiles []string
		wantMgr   string
		wantArgs  []string
	}{
		{"pnpm", []string{"pnpm-lock.yaml"}, "pnpm", []string{"install", "--frozen-lockfile", "--node-linker=hoisted"}},
		{"classic yarn", []string{"yarn.lock"}, "yarn", []string{"install", "--frozen-lockfile"}},
		{"npm ci via package-lock", []string{"package-lock.json"}, "npm", []string{"ci"}},
		{"npm ci via shrinkwrap", []string{"npm-shrinkwrap.json"}, "npm", []string{"ci"}},
		{"no lockfile → npm install without writing a lock", nil, "npm", []string{"install", "--no-package-lock"}},
		{"pnpm wins over yarn + npm", []string{"pnpm-lock.yaml", "yarn.lock", "package-lock.json"}, "pnpm", []string{"install", "--frozen-lockfile", "--node-linker=hoisted"}},
		{"yarn wins over npm", []string{"yarn.lock", "package-lock.json"}, "yarn", []string{"install", "--frozen-lockfile"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, lf := range tc.lockfiles {
				if err := os.WriteFile(filepath.Join(dir, lf), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			mgr, args := installCommand(dir)
			if mgr != tc.wantMgr {
				t.Errorf("manager = %q, want %q", mgr, tc.wantMgr)
			}
			if !reflect.DeepEqual(args, tc.wantArgs) {
				t.Errorf("args = %v, want %v", args, tc.wantArgs)
			}
		})
	}
}

func TestMetadata(t *testing.T) {
	d := &detector{}
	if d.Name() != "node" {
		t.Errorf("Name() = %q, want \"node\"", d.Name())
	}
	if err := d.EnsureToolchain(context.Background()); err != nil {
		t.Errorf("EnsureToolchain() = %v, want nil (Node is baseline)", err)
	}
	if len(d.AllowedHosts()) == 0 {
		t.Error("AllowedHosts() should be non-empty")
	}
	if len(d.VerifyHosts()) == 0 {
		t.Error("VerifyHosts() should be non-empty")
	}
	if err := d.Finalize("/work", nil); err != nil {
		t.Errorf("Finalize() = %v, want nil", err)
	}
}

// TestEnvRedirectsToSharedCache verifies that when cacheDir is set, each
// supported package manager's cache env var points into a subdirectory
// of the shared cache — moving big tarball downloads off the
// memory-backed /home/agent tmpfs.
func TestEnvRedirectsToSharedCache(t *testing.T) {
	d := &detector{}
	got := d.Env("/cache", nil)
	want := []string{
		"YARN_CACHE_FOLDER=/cache/yarn",
		"npm_config_cache=/cache/npm",
		"npm_config_store_dir=/cache/pnpm",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Env(\"/cache\", nil) = %v, want %v", got, want)
	}
}

// TestEnvEmptyCacheDirIsNoop confirms that with no shared cache mounted
// the detector falls back to each package manager's default location.
// Important so standalone agentbox runs (no /cache volume) keep working.
func TestEnvEmptyCacheDirIsNoop(t *testing.T) {
	d := &detector{}
	if got := d.Env("", nil); got != nil {
		t.Errorf("Env(\"\", nil) = %v, want nil (let pkg managers use defaults)", got)
	}
}
