// Package node is the Node.js ecosystem detector for the vendor subcommand.
// It registers itself with internal/vendoring at init time.
package node

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/deployment-io/agentbox/internal/vendoring"
)

func init() { vendoring.Register(&detector{}) }

type detector struct{}

func (*detector) Name() string { return "node" }

// Detect matches a Node project by a package.json at the repo root.
func (*detector) Detect(repoDir string) (bool, error) {
	_, err := os.Stat(filepath.Join(repoDir, "package.json"))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// EnsureToolchain is a no-op: Node, npm and yarn are baked into the agentbox
// image, so they're present in both the vendor and agent phases.
func (*detector) EnsureToolchain(context.Context) error { return nil }

// Vendor installs the project's dependencies into node_modules using the
// package manager its lockfile implies. node_modules lands in the repo dir
// under the bind-mounted /work, so it persists into the agent phase, which
// then runs tsc / build / test offline against it. node_modules is
// conventionally gitignored, so CommitAndPush never stages it.
func (*detector) Vendor(ctx context.Context, repoDir string) error {
	name, args := installCommand(repoDir)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = repoDir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %v in %s: %w", name, args, repoDir, err)
	}
	return nil
}

// installCommand picks the package manager + flags from the lockfile
// present, preferring a frozen/reproducible install that matches the
// committed lockfile. A repo may carry more than one lockfile, so the most
// specific manager wins. The fallback handles any Node project (npm reads
// the standard package.json) WITHOUT writing a package-lock.json, so a
// non-npm project's tree isn't polluted with a stray lockfile that
// CommitAndPush might stage.
//
// Covers npm, classic yarn (1.x), and pnpm. yarn Berry (2+) and bun fall
// through to the npm fallback today — functional, but not lockfile-exact;
// dedicated support is a follow-up.
func installCommand(repoDir string) (string, []string) {
	switch {
	case fileExists(filepath.Join(repoDir, "pnpm-lock.yaml")):
		// hoisted node-linker → a self-contained node_modules in /work that
		// survives into the agent phase. pnpm's default isolated layout keeps
		// real files in a store under $HOME, which is an ephemeral tmpfs here.
		return "pnpm", []string{"install", "--frozen-lockfile", "--node-linker=hoisted"}
	case fileExists(filepath.Join(repoDir, "yarn.lock")):
		return "yarn", []string{"install", "--frozen-lockfile"}
	case fileExists(filepath.Join(repoDir, "package-lock.json")),
		fileExists(filepath.Join(repoDir, "npm-shrinkwrap.json")):
		return "npm", []string{"ci"}
	default:
		return "npm", []string{"install", "--no-package-lock"}
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// AllowedHosts are the public registries the install reaches: registry.npmjs.org
// (npm + pnpm) and registry.yarnpkg.com (classic yarn's default registry).
// Private registries / scoped tokens (.npmrc + NPM_TOKEN) are a follow-up.
func (*detector) AllowedHosts() []string {
	return []string{"registry.npmjs.org", "registry.yarnpkg.com"}
}

// VerifyHosts are the public registries the agent phase may reach to resolve
// verify-time JS deps (and any the agent newly adds). Same as the vendor
// hosts — npm/yarn have no private-git-host equivalent to exclude.
func (*detector) VerifyHosts() []string {
	return []string{"registry.npmjs.org", "registry.yarnpkg.com"}
}

// Env redirects each supported package manager's tarball / content-store
// cache onto the shared cache volume when one is mounted. node_modules
// itself lives in the repo dir under /work and persists into the agent
// phase on its own — that part is unchanged. What this fixes is the
// _intermediate_ download cache: by default yarn/npm/pnpm write to
// ~/.cache/yarn, ~/.npm, ~/.local/share/pnpm/store, all of which land in
// /home/agent — a tmpfs mount sized at 1 GB in the runner spawn. Big
// projects (a Vite app pulls in every-platform @rollup/rollup-* via
// rollup's optionalDependencies, easily >1 GB combined) ENOSPC on
// install. Pointing the caches at the disk-backed /cache named volume
// gets us off tmpfs and incidentally enables cross-Step caching once
// /cache is reused across runs. cacheDir == "" (no shared cache mounted)
// keeps each tool's default location, so single-process / dev runs are
// unaffected.
func (*detector) Env(cacheDir string, _ []string) []string {
	if cacheDir == "" {
		return nil
	}
	return []string{
		// yarn (classic 1.x): standard env var for the tarball cache.
		"YARN_CACHE_FOLDER=" + filepath.Join(cacheDir, "yarn"),
		// npm: maps to the `cache` config key (covers tarballs +
		// metadata). pnpm reads npm_config_* too, so this also moves
		// pnpm's auxiliary cache off /home/agent.
		"npm_config_cache=" + filepath.Join(cacheDir, "npm"),
		// pnpm: its content-addressable store is configured separately
		// from `cache`. Setting npm_config_store_dir is the env-var
		// equivalent of `--store-dir` and avoids per-version flag drift.
		"npm_config_store_dir=" + filepath.Join(cacheDir, "pnpm"),
	}
}

// Finalize: no cross-repo workspace step for Node today (yarn/npm workspaces
// are a follow-up — see PLAN_tasks_verification.md).
func (*detector) Finalize(string, []string) error { return nil }
