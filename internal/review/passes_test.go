package review

import "testing"

var requested = []string{PassSecurity, PassCorrectness}

// The cost gate's whole job: decide when a model call would be spent on
// nothing. Each row below is a change shape and the answer it earns.
func TestSelectPassesCostGate(t *testing.T) {
	for _, tc := range []struct {
		name        string
		paths       []string
		wantPasses  []string
		wantSkipped map[string]string
	}{
		{
			name:        "an ordinary code change runs both passes",
			paths:       []string{"0-acme/api/handler.go", "0-acme/api/README.md"},
			wantPasses:  []string{PassSecurity, PassCorrectness},
			wantSkipped: map[string]string{},
		},
		{
			name:       "documentation only skips both",
			paths:      []string{"0-acme/api/README.md", "0-acme/api/docs/design.mdx", "0-acme/api/LICENSE", "0-acme/api/logo.png"},
			wantPasses: nil,
			wantSkipped: map[string]string{
				PassSecurity:    ReasonDocsOnly,
				PassCorrectness: ReasonDocsOnly,
			},
		},
		{
			name:        "lockfiles only keep security and stand correctness down",
			paths:       []string{"0-acme/api/go.sum", "1-acme/web/package-lock.json", "1-acme/web/yarn.lock"},
			wantPasses:  []string{PassSecurity},
			wantSkipped: map[string]string{PassCorrectness: ReasonLockfileOnly},
		},
		{
			name:       "a lockfile beside real code is an ordinary change",
			paths:      []string{"0-acme/api/go.sum", "0-acme/api/main.go"},
			wantPasses: []string{PassSecurity, PassCorrectness},
			// A dependency bump plus the code that uses it is exactly the
			// change a correctness pass should read.
			wantSkipped: map[string]string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			diff := Diff{Paths: tc.paths}
			passes, skipped := SelectPasses(requested, diff)
			if !equalStrings(passes, tc.wantPasses) {
				t.Errorf("passes = %v, want %v", passes, tc.wantPasses)
			}
			if len(skipped) != len(tc.wantSkipped) {
				t.Errorf("skipped = %v, want %v", skipped, tc.wantSkipped)
			}
			for pass, reason := range tc.wantSkipped {
				if skipped[pass] != reason {
					t.Errorf("skipped[%q] = %q, want %q", pass, skipped[pass], reason)
				}
			}
		})
	}
}

// An empty diff is its own reason, distinct from documentation-only: the PR
// body says which, and the two mean different things to a reader.
func TestSelectPassesSkipsEverythingForAnEmptyDiff(t *testing.T) {
	passes, skipped := SelectPasses(requested, Diff{})
	if len(passes) != 0 {
		t.Errorf("passes = %v, want none", passes)
	}
	for _, pass := range requested {
		if skipped[pass] != ReasonNoChanges {
			t.Errorf("skipped[%q] = %q, want %q", pass, skipped[pass], ReasonNoChanges)
		}
	}
}

// Every parameter appears exactly once, always. A parameter that simply went
// missing from the list would read as "not applicable" rather than "nobody
// looked".
func TestBuildCoverageCoversEveryParameterExactlyOnce(t *testing.T) {
	coverage := BuildCoverage([]string{PassSecurity}, map[string]string{PassCorrectness: ReasonLockfileOnly})
	if len(coverage) != len(allParameters) {
		t.Fatalf("coverage has %d entries, want %d", len(coverage), len(allParameters))
	}
	seen := map[string]int{}
	for _, c := range coverage {
		seen[c.Parameter]++
	}
	for _, parameter := range allParameters {
		if seen[parameter] != 1 {
			t.Errorf("parameter %q appears %d times, want exactly 1", parameter, seen[parameter])
		}
	}

	byParameter := map[string]Coverage{}
	for _, c := range coverage {
		byParameter[c.Parameter] = c
	}
	if got := byParameter["security"]; got.State != stateChecked {
		t.Errorf("security = %+v, want checked", got)
	}
	if got := byParameter["correctness"]; got.State != stateSkipped || got.Reason != ReasonLockfileOnly {
		t.Errorf("correctness = %+v, want skipped with the lockfile reason", got)
	}
	// The six with no pass in this release.
	for _, parameter := range []string{"spec conformance", "testing", "deploy readiness", "performance", "maintainability", "reliability"} {
		if got := byParameter[parameter]; got.State != stateNotChecked || got.Reason != ReasonNoPass {
			t.Errorf("%s = %+v, want not checked", parameter, got)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
