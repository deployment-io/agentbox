package vendoring

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// task pairs a matched detector with the repo directory it matched.
type task struct {
	detector Detector
	repoDir  string
}

// Plan is the set of (detector, repo) vendoring tasks discovered under a
// work directory. Build it with BuildPlan, seed the proxy allowlist with
// AllowedHosts, then run Execute.
type Plan struct {
	tasks []task
}

// BuildPlan walks the immediate subdirectories of workDir (the per-repo
// checkout layout, e.g. /work/0-acme-svc) and records a task for every
// (detector, repo) match. Detection is filesystem-only, so it is safe to
// call before the egress proxy is started.
func BuildPlan(workDir string) (*Plan, error) {
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return nil, fmt.Errorf("read work dir %s: %w", workDir, err)
	}
	p := &Plan{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		repoDir := filepath.Join(workDir, e.Name())
		for _, d := range registry {
			ok, err := d.Detect(repoDir)
			if err != nil {
				return nil, fmt.Errorf("detect %s in %s: %w", d.Name(), repoDir, err)
			}
			if ok {
				p.tasks = append(p.tasks, task{detector: d, repoDir: repoDir})
			}
		}
	}
	return p, nil
}

// Empty reports whether no ecosystems were detected.
func (p *Plan) Empty() bool { return len(p.tasks) == 0 }

// AllowedHosts is the deduplicated, sorted union of every matched
// detector's AllowedHosts — the hosts the vendor phase must reach.
func (p *Plan) AllowedHosts() []string {
	seen := map[string]bool{}
	var hosts []string
	for _, t := range p.tasks {
		for _, h := range t.detector.AllowedHosts() {
			if !seen[h] {
				seen[h] = true
				hosts = append(hosts, h)
			}
		}
	}
	sort.Strings(hosts)
	return hosts
}

// Execute installs each matched toolchain once, then runs every repo's
// vendor step. A toolchain is ensured before the first repo that needs
// it, so a long-tail language's runtime is present before its fetch runs.
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
	return nil
}
