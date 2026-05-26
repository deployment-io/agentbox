package vendoring

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// fakeDetector is a configurable Detector for exercising the registry and
// plan logic without a real toolchain. marker, when set, makes Detect
// match a repo dir containing a file of that name.
type fakeDetector struct {
	name    string
	marker  string
	hosts   []string
	ensures *int
	vendors *int
}

func (f fakeDetector) Name() string { return f.name }

func (f fakeDetector) Detect(repoDir string) (bool, error) {
	if f.marker == "" {
		return false, nil
	}
	_, err := os.Stat(filepath.Join(repoDir, f.marker))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (f fakeDetector) EnsureToolchain(context.Context) error {
	if f.ensures != nil {
		*f.ensures++
	}
	return nil
}

func (f fakeDetector) Vendor(context.Context, string) error {
	if f.vendors != nil {
		*f.vendors++
	}
	return nil
}

func (f fakeDetector) AllowedHosts() []string { return f.hosts }

func TestRegisterDuplicatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate Register")
		}
	}()
	Register(fakeDetector{name: "dup-test"})
	Register(fakeDetector{name: "dup-test"})
}

func TestPlanEmpty(t *testing.T) {
	if !(&Plan{}).Empty() {
		t.Error("empty plan should report Empty() == true")
	}
	p := &Plan{tasks: []task{{detector: fakeDetector{name: "go"}, repoDir: "/a"}}}
	if p.Empty() {
		t.Error("non-empty plan should report Empty() == false")
	}
}

func TestPlanAllowedHostsDedupSorted(t *testing.T) {
	p := &Plan{tasks: []task{
		{detector: fakeDetector{name: "go", hosts: []string{"proxy.golang.org", "github.com"}}, repoDir: "/a"},
		{detector: fakeDetector{name: "node", hosts: []string{"registry.npmjs.org", "github.com"}}, repoDir: "/b"},
	}}
	got := p.AllowedHosts()
	want := []string{"github.com", "proxy.golang.org", "registry.npmjs.org"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AllowedHosts() = %v, want %v", got, want)
	}
}

func TestPlanExecuteEnsuresEachToolchainOnce(t *testing.T) {
	var ensures, vendors int
	d := fakeDetector{name: "go", ensures: &ensures, vendors: &vendors}
	p := &Plan{tasks: []task{
		{detector: d, repoDir: "/a"},
		{detector: d, repoDir: "/b"},
	}}
	if err := p.Execute(context.Background()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if ensures != 1 {
		t.Errorf("EnsureToolchain called %d times, want 1 (once per detector)", ensures)
	}
	if vendors != 2 {
		t.Errorf("Vendor called %d times, want 2 (once per repo)", vendors)
	}
}

func TestBuildPlanDetectsRepoSubdirs(t *testing.T) {
	Register(fakeDetector{name: "buildplan-test", marker: "trigger.marker"})

	work := t.TempDir()
	repo := filepath.Join(work, "0-acme-svc")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "trigger.marker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(work, "1-no-match"), 0o755); err != nil {
		t.Fatal(err)
	}

	p, err := BuildPlan(work)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	matches := 0
	for _, tk := range p.tasks {
		if tk.detector.Name() == "buildplan-test" {
			matches++
			if tk.repoDir != repo {
				t.Errorf("matched repoDir = %q, want %q", tk.repoDir, repo)
			}
		}
	}
	if matches != 1 {
		t.Errorf("buildplan-test matched %d dirs, want 1", matches)
	}
}

func TestConfigureGitEmptyIsNoop(t *testing.T) {
	if err := ConfigureGit(""); err != nil {
		t.Errorf("ConfigureGit(\"\") = %v, want nil", err)
	}
	if err := ConfigureGit("   "); err != nil {
		t.Errorf("ConfigureGit(spaces) = %v, want nil", err)
	}
}
