package review

import (
	"path"
	"strings"
)

// The pass names this release knows. A pass is a focused examination of the
// diff against one review parameter; the runner names the ones it wants in
// REVIEW_PASSES and the cost gate below narrows the list further.
const (
	PassSecurity    = "security"
	PassCorrectness = "correctness"
)

// Every review parameter, in the numbering kit's review_enums uses. agentbox
// deliberately does not import kit — it is a standalone module — so these
// names are a hand-mirror, in the same spirit as the caps in extract.go.
//
// ALL EIGHT appear in every coverage list. The six with no pass report
// NotChecked, which is the whole point of having a coverage list at all: "no
// findings" and "nothing looked" must not be the same answer.
var allParameters = []string{
	"security",
	"correctness",
	"spec conformance",
	"testing",
	"deploy readiness",
	"performance",
	"maintainability",
	"reliability",
}

// parameterForPass maps a pass to the parameter it reports against.
var parameterForPass = map[string]string{
	PassSecurity:    "security",
	PassCorrectness: "correctness",
}

// Coverage state names, as they cross the wire to the runner and on to kit's
// review_enums.ParseCoverageState.
const (
	stateChecked    = "checked"
	stateNotChecked = "not checked"
	stateSkipped    = "skipped"
)

// The reasons the cost gate gives. Pinned as constants because the runner
// renders them into a PR body and a human reads them there: "documentation-only
// change" is an explanation, "skipped" on its own is a shrug.
const (
	ReasonNoChanges    = "no changes in the diff"
	ReasonDocsOnly     = "documentation-only change"
	ReasonLockfileOnly = "lockfile-only change"
	ReasonNoPass       = "no pass for this parameter in this release"
)

// SelectPasses decides which of the requested passes actually run, and why the
// others did not.
//
// This is a COST GATE, not a correctness one. A review run costs a model call
// per Step, and spending it on a change that cannot contain what the pass
// looks for is spending it on nothing:
//
//	no changes at all  — both passes skip; there is nothing to look at.
//	documentation only — both passes skip. A prose change cannot introduce an
//	                     injection or an off-by-one.
//	lockfiles only     — security RUNS (a dependency bump is exactly the shape
//	                     of a supply-chain problem) and correctness skips,
//	                     because a lockfile has no logic to get wrong.
//
// Anything else runs every requested pass. The gate is deliberately narrow:
// it fires only on changes where the skip is obvious, because a pass that
// skips when it should have run reports a clean review of code nobody read.
func SelectPasses(requested []string, diff Diff) (passes []string, skipped map[string]string) {
	skipped = map[string]string{}
	if diff.Empty() {
		for _, p := range requested {
			skipped[p] = ReasonNoChanges
		}
		return nil, skipped
	}
	if allPathsAre(diff.Paths, isDocumentationPath) {
		for _, p := range requested {
			skipped[p] = ReasonDocsOnly
		}
		return nil, skipped
	}
	if allPathsAre(diff.Paths, isLockfilePath) {
		for _, p := range requested {
			if p == PassSecurity {
				passes = append(passes, p)
				continue
			}
			skipped[p] = ReasonLockfileOnly
		}
		return passes, skipped
	}
	return append([]string{}, requested...), skipped
}

// BuildCoverage produces the coverage list: EXACTLY ONE ENTRY PER PARAMETER,
// in a fixed order, so a reader can tell at a glance what was and was not
// examined.
//
// A parameter is Checked when a pass ran for it, Skipped when the cost gate
// stood one down (carrying the gate's reason), and NotChecked when this
// release ships no pass for it at all.
func BuildCoverage(passes []string, skipped map[string]string) []Coverage {
	ran := map[string]bool{}
	for _, p := range passes {
		if parameter, ok := parameterForPass[p]; ok {
			ran[parameter] = true
		}
	}
	skippedByParameter := map[string]string{}
	for p, reason := range skipped {
		if parameter, ok := parameterForPass[p]; ok {
			skippedByParameter[parameter] = reason
		}
	}

	coverage := make([]Coverage, 0, len(allParameters))
	for _, parameter := range allParameters {
		switch {
		case ran[parameter]:
			coverage = append(coverage, Coverage{Parameter: parameter, State: stateChecked})
		case skippedByParameter[parameter] != "":
			coverage = append(coverage, Coverage{Parameter: parameter, State: stateSkipped, Reason: skippedByParameter[parameter]})
		default:
			coverage = append(coverage, Coverage{Parameter: parameter, State: stateNotChecked, Reason: ReasonNoPass})
		}
	}
	return coverage
}

func allPathsAre(paths []string, is func(string) bool) bool {
	if len(paths) == 0 {
		return false
	}
	for _, p := range paths {
		if !is(p) {
			return false
		}
	}
	return true
}

// documentationExtensions and imageExtensions are the file kinds a security or
// correctness pass has nothing to say about.
var documentationExtensions = map[string]bool{
	".md": true, ".mdx": true, ".rst": true, ".txt": true,
}

var imageExtensions = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".svg": true, ".webp": true, ".ico": true, ".bmp": true,
}

// isDocumentationPath reports whether a path is prose or an image — the
// documentation-only gate's unit.
//
// A path under a docs/ directory counts wherever that directory sits, because
// a monorepo's docs live at every level. LICENSE counts by name: it has no
// extension and is the one such file that shows up in real diffs.
func isDocumentationPath(p string) bool {
	p = strings.TrimPrefix(path.Clean(strings.ReplaceAll(p, "\\", "/")), "./")
	base := path.Base(p)
	if base == "LICENSE" || base == "LICENSE.md" || base == "LICENCE" || base == "NOTICE" {
		return true
	}
	ext := strings.ToLower(path.Ext(base))
	if documentationExtensions[ext] || imageExtensions[ext] {
		return true
	}
	for _, segment := range strings.Split(p, "/") {
		if segment == "docs" {
			return true
		}
	}
	return false
}

// lockfileNames are the dependency lockfiles a security pass still wants to
// see — a bumped transitive dependency is a supply-chain change — and a
// correctness pass has nothing to say about.
var lockfileNames = map[string]bool{
	"package-lock.json": true,
	"yarn.lock":         true,
	"pnpm-lock.yaml":    true,
	"go.sum":            true,
	"Cargo.lock":        true,
	"poetry.lock":       true,
	"Gemfile.lock":      true,
	"composer.lock":     true,
}

func isLockfilePath(p string) bool {
	return lockfileNames[path.Base(strings.ReplaceAll(p, "\\", "/"))]
}
