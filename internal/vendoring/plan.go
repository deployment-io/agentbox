package vendoring

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// maxRepoSearchDepth bounds how many directory levels below workDir
// BuildPlan walks looking for a detector match. 2 matches both supported
// layouts: the runner's checkout (/work/<idx>-<owner>/<repo>/, depth 2)
// and flat dev mounts (/work/<repo>/, depth 1). Walking deeper invites
// false matches inside language-specific dependency dirs (node_modules,
// target/, __pycache__, …) on layouts we don't ship.
const maxRepoSearchDepth = 2

// task pairs a matched detector with the repo directory it matched.
type task struct {
	detector Detector
	repoDir  string
}

// Plan is the set of (detector, repo) vendoring tasks discovered under a
// work directory. Build it with BuildPlan, seed the proxy allowlist with
// AllowedHosts/VerifyHosts, apply Env, then run Execute.
type Plan struct {
	workDir string
	tasks   []task
}

// BuildPlan walks the directory tree under workDir up to
// maxRepoSearchDepth levels and records a task for every (detector, repo)
// match. Descent stops at any directory that already matched a detector,
// so a matched repo's interior (sub-packages, vendored deps, node_modules)
// is not searched and cannot double-match. Hidden dirs (e.g. .git, .github)
// are also pruned. The runner's checkout layout puts repos at
// /work/<idx>-<owner>/<repo>/ (depth 2); flat layouts like /work/<repo>/
// (depth 1) are equally supported. Detection is filesystem-only, so it is
// safe to call before the egress proxy is started.
func BuildPlan(workDir string) (*Plan, error) {
	p := &Plan{workDir: workDir}
	if err := discoverRepos(p, workDir, 1); err != nil {
		return nil, err
	}
	return p, nil
}

// discoverRepos walks dir's children, running each registered detector
// against each subdirectory. Matched dirs are recorded and not descended
// into; unmatched dirs recurse up to maxRepoSearchDepth levels deep.
func discoverRepos(p *Plan, dir string, depth int) error {
	if depth > maxRepoSearchDepth {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read dir %s: %w", dir, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		sub := filepath.Join(dir, name)
		matched := false
		for _, d := range registry {
			ok, err := d.Detect(sub)
			if err != nil {
				return fmt.Errorf("detect %s in %s: %w", d.Name(), sub, err)
			}
			if ok {
				p.tasks = append(p.tasks, task{detector: d, repoDir: sub})
				matched = true
			}
		}
		if matched {
			continue
		}
		if err := discoverRepos(p, sub, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// Empty reports whether no ecosystems were detected.
func (p *Plan) Empty() bool { return len(p.tasks) == 0 }

// detectorRepos groups a matched detector with the repo dirs it matched.
type detectorRepos struct {
	detector Detector
	repoDirs []string
}

// distinct returns each matched detector once, paired with all repo dirs it
// matched, in first-seen order.
func (p *Plan) distinct() []detectorRepos {
	var order []string
	byName := map[string]*detectorRepos{}
	for _, t := range p.tasks {
		dr, ok := byName[t.detector.Name()]
		if !ok {
			dr = &detectorRepos{detector: t.detector}
			byName[t.detector.Name()] = dr
			order = append(order, t.detector.Name())
		}
		dr.repoDirs = append(dr.repoDirs, t.repoDir)
	}
	out := make([]detectorRepos, 0, len(order))
	for _, n := range order {
		out = append(out, *byName[n])
	}
	return out
}

// AllowedHosts is the deduplicated, sorted union of every matched detector's
// AllowedHosts — the hosts the VENDOR phase must reach.
func (p *Plan) AllowedHosts() []string {
	return p.unionHosts(func(d Detector) []string { return d.AllowedHosts() })
}

// VerifyHosts is the deduplicated, sorted union of every matched detector's
// VerifyHosts — the public hosts the AGENT phase may reach at verify time.
func (p *Plan) VerifyHosts() []string {
	return p.unionHosts(func(d Detector) []string { return d.VerifyHosts() })
}

func (p *Plan) unionHosts(hosts func(Detector) []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, dr := range p.distinct() {
		for _, h := range hosts(dr.detector) {
			if !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Env is the union of every matched detector's Env — the environment the
// vendor download and the agent verify both need to use the shared cache
// (e.g. GOMODCACHE/GOPRIVATE). Each entry is "KEY=VALUE".
func (p *Plan) Env(cacheDir string) []string {
	var env []string
	for _, dr := range p.distinct() {
		env = append(env, dr.detector.Env(cacheDir, dr.repoDirs)...)
	}
	return env
}

// Execute installs each matched toolchain once, runs every repo's vendor
// step, then runs each detector's Finalize (cross-repo setup, e.g. go.work).
func (p *Plan) Execute(ctx context.Context) error {
	ensured := map[string]bool{}
	for _, t := range p.tasks {
		if !ensured[t.detector.Name()] {
			if err := t.detector.EnsureToolchain(ctx); err != nil {
				return fmt.Errorf("ensure %s toolchain: %w", t.detector.Name(), err)
			}
			ensured[t.detector.Name()] = true
		}
		if err := t.detector.Vendor(ctx, t.repoDir); err != nil {
			return fmt.Errorf("vendor %s in %s: %w", t.detector.Name(), t.repoDir, err)
		}
	}
	for _, dr := range p.distinct() {
		if err := dr.detector.Finalize(p.workDir, dr.repoDirs); err != nil {
			return fmt.Errorf("finalize %s: %w", dr.detector.Name(), err)
		}
	}
	return nil
}
