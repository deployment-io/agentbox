// Package node is the Node.js ecosystem detector for the vendor subcommand.
// It registers itself with internal/vendoring at init time.
package node

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

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

// EnsureToolchain is a no-op: Node, npm, pnpm and the corepack yarn shim are
// baked into the agentbox image, so they're present in both the vendor and
// agent phases.
func (*detector) EnsureToolchain(context.Context) error { return nil }

// Vendor installs the project's dependencies into node_modules using the
// package manager its lockfile implies. node_modules lands in the repo dir
// under the bind-mounted /work, so it persists into the agent phase, which
// then runs tsc / build / test offline against it. node_modules is
// conventionally gitignored, so CommitAndPush never stages it. A Yarn Berry
// repo on the default PnP linker writes .pnp.cjs and .yarn/cache instead of
// node_modules — also in the repo dir, so the same persistence holds and the
// repo's own yarn-wrapped scripts resolve against it.
func (*detector) Vendor(ctx context.Context, repoDir string) error {
	name, args := installCommand(repoDir)
	if name == "yarn" {
		if w := yarnToolchainWarning(repoDir); w != "" {
			fmt.Fprintln(os.Stderr, w)
		}
	}
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
// Covers npm, yarn (classic 1.x and Berry 2+ — see yarnCommand), and pnpm.
// bun falls through to the npm fallback today — functional, but not
// lockfile-exact; dedicated support is a follow-up.
func installCommand(repoDir string) (string, []string) {
	switch {
	case fileExists(filepath.Join(repoDir, "pnpm-lock.yaml")):
		// hoisted node-linker → a self-contained node_modules in /work that
		// survives into the agent phase. pnpm's default isolated layout keeps
		// real files in a store under $HOME, which is an ephemeral tmpfs here.
		return "pnpm", []string{"install", "--frozen-lockfile", "--node-linker=hoisted"}
	case fileExists(filepath.Join(repoDir, "yarn.lock")):
		return yarnCommand(repoDir)
	case fileExists(filepath.Join(repoDir, "package-lock.json")),
		fileExists(filepath.Join(repoDir, "npm-shrinkwrap.json")):
		return "npm", []string{"ci"}
	default:
		return "npm", []string{"install", "--no-package-lock"}
	}
}

// yarnCommand resolves which Yarn runs for repoDir, and with which install
// flags. The two vary independently:
//
//   - The binary. A repo that ships its own release in-tree (a yarnPath in
//     .yarnrc.yml / .yarnrc, or a lone .yarn/releases/*.cjs — how `yarn set
//     version` leaves a 2.x/3.x repo) gets that exact file run under node:
//     no network, no corepack, no version guessing. Everything else goes
//     through the `yarn` shim, which is corepack's (see the Dockerfile), so
//     a package.json packageManager pin resolves to that Yarn and a repo
//     with no pin gets the Classic 1.x default pre-warmed into the image —
//     byte-for-byte today's behaviour for Classic repos.
//
//   - The flags. Berry dropped --frozen-lockfile in favour of --immutable, so
//     the flag has to follow the version that is actually about to run — not
//     the lockfile format, and not merely whether a release is vendored. A
//     repo mid-migration (Classic lockfile, packageManager pinned to
//     yarn@4) runs Berry, so it gets --immutable.
//
//     Getting this wrong is silent in the dangerous direction. Berry still
//     accepts --frozen-lockfile (it warns YN0050 and honours it), but Classic
//     does not reject --immutable — it ignores the unknown flag and installs
//     UNFROZEN, rewriting yarn.lock. The runner stages the repo with
//     AddGlob("."), so that rewrite rides into the Task's commit as an
//     unrelated change, with the lockfile check the flag exists to perform
//     never having run. Hence --immutable only on a confirmed Berry, and
//     --frozen-lockfile — the flag both majors honour — whenever the version
//     can't be established.
//
// Note what is deliberately NOT a signal here: a Berry lockfile on its own.
// Nothing in the image can act on it — with no pin and no vendored release
// corepack still runs Classic — so it selects today's Classic invocation and
// yarnToolchainWarning explains the failure ahead of it.
func yarnCommand(repoDir string) (string, []string) {
	release := vendoredYarnRelease(repoDir)
	args := []string{"install", "--frozen-lockfile"}
	if resolvedYarnMajor(repoDir, release) >= 2 {
		args = []string{"install", "--immutable"}
	}
	if release != "" {
		return "node", append([]string{release}, args...)
	}
	return "yarn", args
}

// resolvedYarnMajor reports the major version of the Yarn that will actually
// run for repoDir, given the release vendoredYarnRelease picked (possibly "").
//
// A vendored release decides it alone: that file IS the binary about to be
// exec'd, so a packageManager pin naming some other major is not what runs and
// must not pick the flags. `yarn policies set-version` on Yarn 1 vendors a
// Classic release exactly this way, which is why "a release is present" cannot
// stand in for "this repo is Berry".
//
// 0 means undetermined — no release, no pin, or a release whose filename
// carries no version — and callers treat that as corepack's Classic default.
func resolvedYarnMajor(repoDir, release string) int {
	if release != "" {
		return releaseMajor(release)
	}
	return declaredYarnMajor(repoDir)
}

// releaseMajor returns the Yarn major named by a vendored release's filename
// — yarn-4.9.4.cjs -> 4, yarn-1.22.19.js -> 1 — or 0 when the name doesn't
// carry one. `yarn set version` and `yarn policies set-version` both write
// this yarn-<version>.<ext> shape; anything else is treated as unknown rather
// than guessed at, which costs only the deprecated-but-honoured flag.
func releaseMajor(path string) int {
	name := filepath.Base(path)
	name = strings.TrimSuffix(name, filepath.Ext(name))
	rest, ok := strings.CutPrefix(name, "yarn-")
	if !ok {
		return 0
	}
	major, _, _ := strings.Cut(rest, ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		return 0
	}
	return n
}

// vendoredYarnRelease returns the path of the Yarn release repoDir ships
// in-tree, or "" if it ships none. An explicit yarnPath wins over whatever
// happens to sit in .yarn/releases: .yarnrc.yml is Berry's config, .yarnrc
// is Classic's, which Berry repos still use to redirect an otherwise
// unmigrated toolchain. A yarnPath naming a file that isn't there is
// ignored rather than trusted — better the shim than a guaranteed ENOENT.
// Multiple vendored releases are ambiguous; the highest-sorting name wins,
// which is the newest release for any conventional yarn-<version>.cjs set.
func vendoredYarnRelease(repoDir string) string {
	for _, cfg := range []struct{ file, key string }{
		{".yarnrc.yml", "yarnPath:"},
		{".yarnrc", "yarn-path"},
	} {
		p := configValue(filepath.Join(repoDir, cfg.file), cfg.key)
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(repoDir, p)
		}
		if regularFile(p) {
			return p
		}
	}
	matches, _ := filepath.Glob(filepath.Join(repoDir, ".yarn", "releases", "*.cjs"))
	sort.Strings(matches)
	for i := len(matches) - 1; i >= 0; i-- {
		if regularFile(matches[i]) {
			return matches[i]
		}
	}
	return ""
}

// declaredYarnMajor returns the major version of the Yarn pinned in
// package.json's packageManager field — corepack's pin, e.g.
// "yarn@4.1.0+sha224.abc". 0 when the field is absent, names another
// package manager, or doesn't parse.
func declaredYarnMajor(repoDir string) int {
	b, err := os.ReadFile(filepath.Join(repoDir, "package.json"))
	if err != nil {
		return 0
	}
	var pkg struct {
		PackageManager string `json:"packageManager"`
	}
	if err := json.Unmarshal(b, &pkg); err != nil {
		return 0
	}
	spec, ok := strings.CutPrefix(strings.TrimSpace(pkg.PackageManager), "yarn@")
	if !ok {
		return 0
	}
	major, _, _ := strings.Cut(spec, ".")
	major, _, _ = strings.Cut(major, "+")
	n, err := strconv.Atoi(major)
	if err != nil {
		return 0
	}
	return n
}

// yarnLockIsBerry reports whether repoDir's yarn.lock was written by Yarn
// Berry. Berry lockfiles are YAML carrying a `__metadata:` block (version 4
// through 8 across 2.x–4.x) a few lines in; Classic's are a bespoke format
// headed by "# yarn lockfile v1". Only the head of the file is read —
// lockfiles run to megabytes.
func yarnLockIsBerry(repoDir string) bool {
	f, err := os.Open(filepath.Join(repoDir, "yarn.lock"))
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, 8<<10))
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "__metadata:") {
			return true
		}
	}
	return false
}

// yarnToolchainWarning returns a one-line diagnostic for the Yarn shape the
// image cannot resolve on the repo's behalf: a Berry lockfile that resolves to
// Yarn Classic anyway. The install is left to fail exactly as it does today —
// Classic cannot parse a Berry lockfile and aborts — but preceded by a line
// naming the cause and the repo-side fix, instead of Classic's bare "your
// lockfile needs to be updated".
//
// Two ways in, and the second is the one that bit deployment-io/website-svc:
//
//   - Nothing pinned at all. corepack has no packageManager field to key off,
//     so it runs its Classic default.
//   - A vendored CLASSIC release. `yarn policies set-version` on Yarn 1 writes
//     .yarn/releases/yarn-1.22.x.js plus a .yarnrc yarn-path, and that
//     redirect long outlives a migration to Berry — leaving a repo whose
//     lockfile is Berry and whose pinned toolchain is not.
//
// Empty when the resolved Yarn is Berry, which is the normal case: `yarn set
// version` writes a packageManager pin (Berry 3.1+) or vendors a Berry release
// (2.x/3.x).
func yarnToolchainWarning(repoDir string) string {
	if !yarnLockIsBerry(repoDir) {
		return ""
	}
	release := vendoredYarnRelease(repoDir)
	if resolvedYarnMajor(repoDir, release) >= 2 {
		return ""
	}
	cause := "pins no Yarn version: no packageManager field in package.json " +
		"and no vendored .yarn/releases release"
	if release != "" {
		cause = fmt.Sprintf("pins Yarn Classic: %s", release)
	}
	return fmt.Sprintf("agentbox: %s has a Yarn Berry lockfile but %s. "+
		"The install below runs under Yarn Classic, which cannot read a Berry "+
		"lockfile. Fix in the repo with `yarn set version <version>`.", repoDir, cause)
}

// configValue returns the value of a top-level `key` line in a yarn config
// file — `yarnPath: .yarn/releases/yarn-4.1.0.cjs` (.yarnrc.yml) or
// `yarn-path "./.yarn/releases/yarn-1.22.19.cjs"` (.yarnrc) — unquoted, or
// "" if the file or key is absent. Matching only at column 0 keeps nested
// YAML keys of the same name out of the result; this is deliberately not a
// YAML parse, since one key of one shape is all that's needed.
func configValue(path, key string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, 64<<10))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, key) {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(line, key))
		return strings.Trim(v, `"'`)
	}
	return ""
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func regularFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// AllowedHosts are the public registries the install reaches: registry.npmjs.org
// (npm + pnpm), registry.yarnpkg.com (classic yarn's default registry), and
// repo.yarnpkg.com, where corepack fetches the Yarn release a repo's
// packageManager field pins. corepack resolves some versions through the npm
// registry instead, so both are needed; missing repo.yarnpkg.com shows up as
// the proxy stalling the install rather than as a named denial.
// Private registries / scoped tokens (.npmrc + NPM_TOKEN) are a follow-up.
func (*detector) AllowedHosts() []string {
	return []string{"registry.npmjs.org", "registry.yarnpkg.com", "repo.yarnpkg.com"}
}

// VerifyHosts are the public registries the agent phase may reach to resolve
// verify-time JS deps (and any the agent newly adds). Same as the vendor
// hosts — npm/yarn have no private-git-host equivalent to exclude, and the
// agent phase runs the same corepack-backed `yarn`.
func (*detector) VerifyHosts() []string {
	return []string{"registry.npmjs.org", "registry.yarnpkg.com", "repo.yarnpkg.com"}
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
		// Berry honours it too (it maps YARN_* onto its own config keys,
		// here cacheFolder) for repos that keep the per-project cache.
		"YARN_CACHE_FOLDER=" + filepath.Join(cacheDir, "yarn"),
		// yarn (berry): with enableGlobalCache — the default since Yarn 4 —
		// cacheFolder is bypassed for a cache under globalFolder, which
		// defaults into /home/agent, i.e. straight onto the 1 GB tmpfs the
		// var above exists to avoid. Classic reads global-folder only for
		// `yarn global`, which the vendor step never runs, so this is inert
		// for Classic repos.
		"YARN_GLOBAL_FOLDER=" + filepath.Join(cacheDir, "yarn-berry"),
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
