package claude

import (
	"slices"
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/config"
)

// The session prompt asks the agent to "grep/jq only the files relevant" in
// /work/context, whose files are JSON, and the image ships jq — but the tool
// the session is held to was never on the allowlist, so every one of those
// calls came back a permission denial with no human to prompt. A read-only
// session that cannot read the one directory it was pointed at spends its
// turns discovering that.
func TestReadOnlyAllowlistCarriesJq(t *testing.T) {
	if !slices.Contains(readOnlyAllowedTools, "Bash(jq *)") {
		t.Errorf("the read-only allowlist has no jq entry, so every jq call in a session "+
			"or a review is denied: %v", readOnlyAllowedTools)
	}
	// The whole list is still side-effect free: jq reads a file and prints to
	// stdout, which is the exposure `Bash(cat *)` already carries.
	args := (&Driver{}).BuildInteractiveArgs(&config.Config{Mode: config.ModeInteractive, ReadOnly: true})
	if !slices.Contains(args, "Bash(jq *)") {
		t.Errorf("a read-only session was not given the jq entry: %v", args)
	}
	if slices.Contains(args, "--dangerously-skip-permissions") {
		t.Error("read-only must NOT skip permissions; the allowlist would be bypassed")
	}
}

// And the entry must not disturb what verified read-only mounts do to the
// list: every Bash pattern, jq's included, collapses into ONE plain Bash. A
// second entry that survived beside it would be dead weight at best and, if
// it were ever a non-Bash form, a restriction the review no longer applies.
func TestJqEntryStillCollapsesOnVerifiedReadOnlyMounts(t *testing.T) {
	allowed := reviewAllowedTools(true)
	if n := slices.Index(allowed, "Bash"); n < 0 {
		t.Fatalf("verified read-only mounts did not grant a plain Bash: %v", allowed)
	}
	var bashEntries int
	for _, entry := range allowed {
		if strings.HasPrefix(entry, "Bash") {
			bashEntries++
		}
		if strings.HasPrefix(entry, "Bash(") {
			t.Errorf("a restricted pattern %q survived the collapse: %v", entry, allowed)
		}
	}
	if bashEntries != 1 {
		t.Errorf("Bash entries = %d, want exactly one: %v", bashEntries, allowed)
	}
	// Without the mounts the list is passed through untouched, jq and all.
	if got := reviewAllowedTools(false); !slices.Equal(got, readOnlyAllowedTools) {
		t.Errorf("reviewAllowedTools(false) = %v, want the read-only list %v", got, readOnlyAllowedTools)
	}
}
