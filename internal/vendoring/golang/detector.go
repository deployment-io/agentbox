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
	"regexp"
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

// Vendor populates the shared module cache with the repo's build/test
// dependencies (the module build list — sufficient for an offline
// `go build`/`go test ./...`, and lighter than `download all`, which also
// tends to rewrite go.sum). GOMODCACHE (set via Env) points at the shared
// shelf; GOWORK=off forces standalone resolution so a checked-out repo
// vendors the same set it would resolve in CI, not against an ambient
// workspace.
func (*detector) Vendor(ctx context.Context, repoDir string) error {
	cmd := exec.CommandContext(ctx, "go", "mod", "download")
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
//   - storage.googleapis.com — the GCS backend proxy.golang.org serves
//     module zips from; without it, downloads fail mid-fetch
//   - github.com / objects.githubusercontent.com — direct fetch of
//     GOPRIVATE modules (e.g. github.com/deployment-io) and their archives
func (*detector) AllowedHosts() []string {
	return []string{
		"proxy.golang.org",
		"sum.golang.org",
		"storage.googleapis.com",
		"github.com",
		"objects.githubusercontent.com",
	}
}

// VerifyHosts are the public hosts the agent phase may reach to resolve
// verify-time Go deps — the module proxy, its checksum db, and the GCS
// backend it serves zips from. github.com is deliberately excluded: private
// modules are pre-vendored on the shared cache and the agent holds no token
// to fetch them directly.
func (*detector) VerifyHosts() []string {
	return []string{"proxy.golang.org", "sum.golang.org", "storage.googleapis.com"}
}

// goWorkspaceFloor is the lowest `go` directive a generated go.work carries.
// The directive actually written is the highest `go` line among the in-Step
// modules (see workspaceGoVersion): with a go.work present the go command
// takes its version requirement from go.work, not from the modules, so a
// go.work older than any module fails every go command with "module requires
// go >= X, but go.work lists Y". A hard-coded value here rotted exactly that
// way when the repos moved to Go 1.25 while this still said 1.24.11. The
// floor only matters when no module declares a version at all.
const goWorkspaceFloor = "1.24"

// goDirective matches the `go` line of a go.mod: `go 1.25.14`, `go 1.24`,
// `go 1.26rc1`.
var goDirective = regexp.MustCompile(`(?m)^go[ \t]+(\S+)`)

// workspaceGoVersion returns the highest go directive across repoDirs' go.mod
// files, or goWorkspaceFloor if none is found. A repo without a go.mod (or an
// unreadable one) is skipped — Finalize is only called for dirs the detector
// matched, and a missing directive is a legal, if archaic, go.mod.
func workspaceGoVersion(repoDirs []string) string {
	best := goWorkspaceFloor
	for _, dir := range repoDirs {
		data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err != nil {
			continue
		}
		m := goDirective.FindSubmatch(data)
		if m == nil {
			continue
		}
		if v := string(m[1]); compareGoVersions(v, best) > 0 {
			best = v
		}
	}
	return best
}

// compareGoVersions orders go directive versions numerically component by
// component ("1.25.14" > "1.25.2" > "1.25" > "1.24"). A pre-release suffix
// ("1.26rc1") is compared as the numeric prefix it carries; that's enough to
// pick a workspace version, which never needs to distinguish rc from final.
func compareGoVersions(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x = leadingInt(pa[i])
		}
		if i < len(pb) {
			y = leadingInt(pb[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// leadingInt parses the digits at the start of s ("14" → 14, "26rc1" → 26).
func leadingInt(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// Env points the toolchain at the shared cache and marks the in-Step repos'
// module owners as private (direct git via the vendor token, not the public
// proxy). When cacheDir is set (the runner-spawned production case), every
// cache-like env var — module cache, build cache, and the per-invocation
// scratch dir — moves onto the disk-backed /cache volume. The build cache
// in particular previously lived on /tmp (a 512 MB tmpfs); a kit-scale dep
// graph fills that during `go build ./...` and the resulting ENOSPC
// cascades into every subsequent shell command failing because /tmp is
// also where bash/coreutils write scratch — mirrors the v1.3.2 Node cache
// redirect, same shape, different ecosystem. When cacheDir is "" (no
// shared cache mounted, standalone runs) the defaults stand so the
// detector stays usable in dev/integration setups without a /cache volume.
func (*detector) Env(cacheDir string, repoDirs []string) []string {
	var env []string
	if cacheDir != "" {
		// Pre-create the cache subdirs before anyone consumes the env.
		// `go build` requires GOCACHE and GOTMPDIR to exist (it
		// MkdirAll's the per-invocation b<N> tree underneath but does
		// not create the parent); if either is missing the build aborts
		// before doing useful work with a misleading "mkdir" error
		// pointing at the temp tree rather than at GOTMPDIR itself.
		// GOMODCACHE is auto-created by `go mod download` so it isn't
		// strictly necessary, but a symmetric MkdirAll keeps the
		// invariant obvious and survives partial /cache wipes. Errors
		// are intentionally swallowed — if MkdirAll fails here the
		// subsequent go invocation will surface a real, actionable
		// permission/ENOSPC error pointing at the env-var value.
		for _, sub := range []string{"gomod", "gobuild", "gotmp"} {
			_ = os.MkdirAll(filepath.Join(cacheDir, sub), 0o755)
		}
		env = append(env,
			"GOMODCACHE="+filepath.Join(cacheDir, "gomod"),
			// GOCACHE = persistent compiled-artifact cache (survives
			// across Steps once /cache is reused).
			"GOCACHE="+filepath.Join(cacheDir, "gobuild"),
			// GOTMPDIR = where `go build` writes per-invocation scratch
			// (importcfg, link inputs, the b<N> tree). Not reused
			// across builds; just needs disk room.
			"GOTMPDIR="+filepath.Join(cacheDir, "gotmp"),
		)
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
	fmt.Fprintf(&b, "go %s\n\nuse (\n", workspaceGoVersion(repoDirs))
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
