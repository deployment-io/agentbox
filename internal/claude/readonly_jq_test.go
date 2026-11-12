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

// A session's working directory is not a repository and plain git cannot be
// pointed at one, so repo-git is the only way a read-only session reads
// history — and the prompt has to say so, word for word, only when read-only.
func TestReadOnlySessionGetsRepoGit(t *testing.T) {
	if !slices.Contains(readOnlyAllowedTools, "Bash(agentbox repo-git *)") {
		t.Errorf("the read-only allowlist has no repo-git entry: %v", readOnlyAllowedTools)
	}
	for _, entry := range []string{"Bash(git log *)", "Bash(git diff *)", "Bash(git show *)", "Bash(git status *)", "Bash(git blame *)"} {
		if !slices.Contains(readOnlyAllowedTools, entry) {
			t.Errorf("the read-only allowlist lost %s", entry)
		}
	}
	d := &Driver{}
	args := d.BuildInteractiveArgs(&config.Config{Mode: config.ModeInteractive, ReadOnly: true, AppendSystemPrompt: "base"})
	if !slices.Contains(args, "Bash(agentbox repo-git *)") {
		t.Errorf("a read-only session was not given repo-git: %v", args)
	}
	if v, _ := argValue(args, "--append-system-prompt"); v != "base\n\n"+readOnlyGitPrompt {
		t.Errorf("--append-system-prompt = %q", v)
	}
	args = d.BuildInteractiveArgs(&config.Config{Mode: config.ModeInteractive, ReadOnly: true})
	if v, _ := argValue(args, "--append-system-prompt"); v != readOnlyGitPrompt {
		t.Errorf("empty AppendSystemPrompt: --append-system-prompt = %q", v)
	}
	if !strings.HasPrefix(readOnlyGitPrompt, "Git: a plain git command is refused here") ||
		!strings.HasSuffix(readOnlyGitPrompt, "work with what you can run.") {
		t.Errorf("the paragraph changed: %q", readOnlyGitPrompt)
	}

	args = d.BuildInteractiveArgs(&config.Config{Mode: config.ModeInteractive, AppendSystemPrompt: "base"})
	if v, _ := argValue(args, "--append-system-prompt"); v != "base" {
		t.Errorf("a non-read-only session's prompt changed: %q", v)
	}
	if args := d.BuildInteractiveArgs(&config.Config{Mode: config.ModeInteractive}); hasArg(args, "--append-system-prompt") {
		t.Errorf("a non-read-only session without a prompt got one: %v", args)
	}
}
