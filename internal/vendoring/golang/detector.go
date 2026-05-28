// Package golang is the Go ecosystem detector for the vendor subcommand.
// It registers itself with internal/vendoring at init time.
package golang

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/deployment-io/agentbox/internal/vendoring"
)

func init() { vendoring.Register(&detector{}) }

type detector struct{}

func (*detector) Name() string { return "go" }

// Detect matches a Go module by a go.mod at the repo root.
func (*detector) Detect(repoDir string) (bool, error) {
	_, err := os.Stat(filepath.Join(repoDir, "go.mod"))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// EnsureToolchain is a no-op: Go is a baseline toolchain baked into the
// agentbox image, so it is present in both the vendor and agent phases.
func (*detector) EnsureToolchain(context.Context) error { return nil }

// Vendor populates the shared module cache with the repo's full module
// graph. GOMODCACHE (set via Env) points at the shared shelf; GOWORK=off
// forces standalone resolution so a checked-out repo vendors the same set
// it would resolve in CI, not against an ambient workspace.
func (*detector) Vendor(ctx context.Context, repoDir string) error {
	cmd := exec.CommandContext(ctx, "go", "mod", "download", "all")
	cmd.Dir = repoDir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go mod download in %s: %w", repoDir, err)
	}
	return nil
}

// AllowedHosts are the hosts `go mod download` reaches:
//   - proxy.golang.org / sum.golang.org — public module proxy + checksum db
//   - github.com / objects.githubusercontent.com — direct fetch of
//     GOPRIVATE modules (e.g. github.com/deployment-io) and their archives
func (*detector) AllowedHosts() []string {
	return []string{
		"proxy.golang.org",
		"sum.golang.org",
		"github.com",
		"objects.githubusercontent.com",
	}
}

// VerifyHosts are the public hosts the agent phase may reach to resolve
// verify-time Go deps — the module proxy + checksum db only. github.com is
// deliberately excluded: private modules are pre-vendored on the shared
// cache and the agent holds no token to fetch them directly.
func (*detector) VerifyHosts() []string {
	return []string{"proxy.golang.org", "sum.golang.org"}
}

// goBuildCache is GOCACHE (compiled-artifact cache) on tmpfs — never the
// shared module shelf.
const goBuildCache = "/tmp/gocache"

// goWorkspaceVersion is the `go` directive written into a generated go.work.
// Must be >= every in-Step module's go directive; tracks the Go baked into
// the agentbox image (GO_VERSION in the Dockerfile) — bump together.
const goWorkspaceVersion = "1.24.11"

// Env points the toolchain at the shared module cache and marks the in-Step
// repos' module owners as private (direct git via the vendor token, not the
// public proxy). cacheDir is "" when no shared cache is mounted, in which
// case Go falls back to its default module cache.
func (*detector) Env(cacheDir string, repoDirs []string) []string {
	env := []string{"GOCACHE=" + goBuildCache}
	if cacheDir != "" {
		env = append(env, "GOMODCACHE="+filepath.Join(cacheDir, "gomod"))
	}
	if gp := goPrivate(repoDirs); gp != "" {
		env = append(env, "GOPRIVATE="+gp)
	}
	return env
}

// Finalize writes /work/go.work listing every in-Step Go module, but only
// when 2+ are present — so the agent's verify resolves cross-module edits
// against local source instead of pinned cached versions. The file sits at
// the work-dir root, outside any repo's git tree, so CommitAndPush never
// stages it.
func (*detector) Finalize(workDir string, repoDirs []string) error {
	if len(repoDirs) < 2 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "go %s\n\nuse (\n", goWorkspaceVersion)
	for _, dir := range repoDirs {
		rel, err := filepath.Rel(workDir, dir)
		if err != nil {
			rel = filepath.Base(dir)
		}
		fmt.Fprintf(&b, "\t./%s\n", rel)
	}
	b.WriteString(")\n")
	return os.WriteFile(filepath.Join(workDir, "go.work"), []byte(b.String()), 0o644)
}

// goPrivate derives a comma-separated GOPRIVATE from the module path declared
// in each repo's go.mod (host/owner), deduped — authoritative for what counts
// as private, vs parsing clone URLs.
func goPrivate(repoDirs []string) string {
	seen := map[string]struct{}{}
	var prefixes []string
	for _, dir := range repoDirs {
		ho := moduleHostOwner(dir)
		if ho == "" {
			continue
		}
		if _, ok := seen[ho]; ok {
			continue
		}
		seen[ho] = struct{}{}
		prefixes = append(prefixes, ho)
	}
	return strings.Join(prefixes, ",")
}

// moduleHostOwner reads the `module` path from repoDir/go.mod and returns its
// host/owner prefix (e.g. github.com/acme from github.com/acme/svc). "" if
// unreadable or not a 2+-segment path.
func moduleHostOwner(repoDir string) string {
	f, err := os.Open(filepath.Join(repoDir, "go.mod"))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "module ") {
			continue
		}
		modPath := strings.TrimSpace(strings.TrimPrefix(line, "module"))
		if parts := strings.Split(modPath, "/"); len(parts) >= 2 {
			return parts[0] + "/" + parts[1]
		}
		return ""
	}
	return ""
}
