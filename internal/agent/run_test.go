package agent

import (
	"os/exec"
	"strings"
	"testing"
)

// realExitError returns a genuine *exec.ExitError ("exit status 1") so
// failureMessage's isExitError branch is exercised against the real type
// rather than a stand-in. sh is present on both the Linux CI image and
// macOS dev machines.
func realExitError(t *testing.T) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit 1").Run()
	if err == nil {
		t.Fatal("expected a non-nil exit error from `sh -c exit 1`")
	}
	return err
}

func TestFailureMessage_PrefersFailureReason(t *testing.T) {
	// Even with a real exit error present, a structured FailureReason
	// (e.g. max turns) wins — it's the actionable explanation.
	state := ParsedState{
		IsError:       true,
		FailureReason: "reached its turn limit after 26 turns; raise max_turns to allow more steps",
	}
	got := failureMessage(realExitError(t), state, "ignored stderr", "claude")

	if !strings.HasPrefix(got, "claude ") {
		t.Errorf("message should be prefixed with the binary name: %q", got)
	}
	if !strings.Contains(got, "turn limit after 26 turns") || !strings.Contains(got, "max_turns") {
		t.Errorf("message should carry the turn-limit reason: %q", got)
	}
	if strings.Contains(got, "exit status 1") {
		t.Errorf("structured reason should replace the raw exit status: %q", got)
	}
}

func TestFailureMessage_PrefersAgentDescriptionOverExitStatus(t *testing.T) {
	// The agent exited non-zero but its result event described the
	// failure. That description must win over a bare "exit status 1" —
	// the exit error fires for the same run and says nothing useful.
	state := ParsedState{
		IsError:        true,
		ChangesSummary: "build failed: undefined symbol Foo",
		ErrorSubtype:   "error_during_execution",
	}
	got := failureMessage(realExitError(t), state, "unrelated stderr noise", "claude")

	if !strings.Contains(got, "build failed: undefined symbol Foo") {
		t.Errorf("want the agent's own error description, got %q", got)
	}
	if strings.Contains(got, "exit status 1") {
		t.Errorf("raw exit status should not win over the description: %q", got)
	}
}

func TestFailureMessage_NamesSubtypeWhenNoDescription(t *testing.T) {
	// Classified error, no description, no stderr — name the class so the
	// subtype is never silently dropped from the surfaced error.
	state := ParsedState{IsError: true, ErrorSubtype: "error_during_execution"}
	got := failureMessage(realExitError(t), state, "", "claude")

	if !strings.Contains(got, "error_during_execution") {
		t.Errorf("want the subtype named when there's no other detail, got %q", got)
	}
}

func TestFailureMessage_ExitErrorAppendsStderrTail(t *testing.T) {
	// No FailureReason: the agent died before a result event. The only
	// useful detail is in stderr, so it should be appended to the
	// otherwise-useless "exit status 1".
	stderr := "npm warn deprecated foo@1.0.0\nError: Cannot find module '@anthropic-ai/claude-code'\n\n"
	got := failureMessage(realExitError(t), ParsedState{}, stderr, "claude")

	if !strings.Contains(got, "exited with error") {
		t.Errorf("want the exit-error framing: %q", got)
	}
	if !strings.Contains(got, "Cannot find module") {
		t.Errorf("want the stderr tail appended: %q", got)
	}
}

func TestFailureMessage_ExitErrorNoStderrIsUnchanged(t *testing.T) {
	// Whitespace-only stderr yields no tail; message stays the bare
	// (but still binary-named) exit error.
	got := failureMessage(realExitError(t), ParsedState{}, "  \n\t\n", "claude")
	if !strings.Contains(got, "claude exited with error") {
		t.Errorf("want bare exit-error message: %q", got)
	}
	if strings.Contains(got, "—") {
		t.Errorf("no stderr tail should mean no separator: %q", got)
	}
}

func TestStderrTail(t *testing.T) {
	if got := stderrTail("first\nlast useful line\n\n  \n"); got != "last useful line" {
		t.Errorf("stderrTail = %q, want last non-empty line", got)
	}
	if got := stderrTail(""); got != "" {
		t.Errorf("stderrTail(\"\") = %q, want empty", got)
	}
	if got := stderrTail("   \n\t\n"); got != "" {
		t.Errorf("stderrTail of blank lines = %q, want empty", got)
	}
	long := strings.Repeat("x", 300)
	got := stderrTail(long)
	if r := []rune(got); len(r) != 201 || !strings.HasSuffix(got, "…") {
		t.Errorf("stderrTail should cap at 200 runes + ellipsis, got len=%d", len([]rune(got)))
	}
}
