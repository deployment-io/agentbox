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
	name        string
	marker      string
	hosts       []string
	verifyHosts []string
	env         []string
	ensures     *int
	vendors     *int
	finalizes   *int
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

func (f fakeDetector) VerifyHosts() []string { return f.verifyHosts }

func (f fakeDetector) Env(string, []string) []string { return f.env }

func (f fakeDetector) Finalize(string, []string) error {
	if f.finalizes != nil {
		*f.finalizes++
	}
	return nil
}

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
	var ensures, vendors, finalizes int
	d := fakeDetector{name: "go", ensures: &ensures, vendors: &vendors, finalizes: &finalizes}
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
	if finalizes != 1 {
		t.Errorf("Finalize called %d times, want 1 (once per detector)", finalizes)
	}
}

func TestPlanVerifyHostsAndEnv(t *testing.T) {
	p := &Plan{tasks: []task{
		{detector: fakeDetector{name: "go", verifyHosts: []string{"proxy.golang.org"}, env: []string{"GOFLAG=1"}}, repoDir: "/a"},
		{detector: fakeDetector{name: "node", verifyHosts: []string{"registry.npmjs.org"}}, repoDir: "/b"},
	}}
	if got, want := p.VerifyHosts(), []string{"proxy.golang.org", "registry.npmjs.org"}; !reflect.DeepEqual(got, want) {
		t.Errorf("VerifyHosts() = %v, want %v", got, want)
	}
	if got, want := p.Env(""), []string{"GOFLAG=1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Env() = %v, want %v", got, want)
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

// matchedDirs returns the repo dirs the given detector name matched in p.
func matchedDirs(p *Plan, name string) []string {
	var out []string
	for _, tk := range p.tasks {
		if tk.detector.Name() == name {
			out = append(out, tk.repoDir)
		}
	}
	return out
}

// TestBuildPlanRunnerLayout exercises the runner's two-level checkout
// layout (/work/<idx>-<owner>/<repo>/) — the case the original
// single-level walk silently missed.
func TestBuildPlanRunnerLayout(t *testing.T) {
	Register(fakeDetector{name: "buildplan-runner", marker: "go.mod.marker"})

	work := t.TempDir()
	repo := filepath.Join(work, "0-deployment-io", "team-ai")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "go.mod.marker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	p, err := BuildPlan(work)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	got := matchedDirs(p, "buildplan-runner")
	if want := []string{repo}; !reflect.DeepEqual(got, want) {
		t.Errorf("matched = %v, want %v", got, want)
	}
}

// TestBuildPlanStopsAtMatch ensures a matched repo's interior isn't
// searched — a nested go.mod inside a matched parent must not double-match.
func TestBuildPlanStopsAtMatch(t *testing.T) {
	Register(fakeDetector{name: "buildplan-stop", marker: "stop.marker"})

	work := t.TempDir()
	repo := filepath.Join(work, "svc")
	nested := filepath.Join(repo, "sub-pkg")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{repo, nested} {
		if err := os.WriteFile(filepath.Join(dir, "stop.marker"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	p, err := BuildPlan(work)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	got := matchedDirs(p, "buildplan-stop")
	if want := []string{repo}; !reflect.DeepEqual(got, want) {
		t.Errorf("matched = %v, want %v (outer repo only — must not descend into matched parent)", got, want)
	}
}

// TestBuildPlanSkipsHidden confirms dotfile directories (.git, .github, …)
// are pruned during discovery — universal OS convention, applies regardless
// of which language detectors are registered.
func TestBuildPlanSkipsHidden(t *testing.T) {
	Register(fakeDetector{name: "buildplan-skip", marker: "skip.marker"})

	work := t.TempDir()
	hidden := filepath.Join(work, ".git", "objects")
	if err := os.MkdirAll(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hidden, "skip.marker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	p, err := BuildPlan(work)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if got := matchedDirs(p, "buildplan-skip"); len(got) != 0 {
		t.Errorf("matched = %v, want none (hidden dirs must be pruned)", got)
	}
}
