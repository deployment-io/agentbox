package claude

import (
	"strings"
	"testing"
)

func TestSplitVerifyTrailer(t *testing.T) {
	t.Run("passed", func(t *testing.T) {
		summary, vr := splitVerifyTrailer("Did the thing.\n\n<verify>{\"ran\":true,\"passed\":true,\"command\":\"go build ./...\"}</verify>")
		if summary != "Did the thing." {
			t.Errorf("summary = %q, want the <verify> tag stripped", summary)
		}
		if vr == nil || !vr.Ran || !vr.Passed || vr.Command != "go build ./..." {
			t.Errorf("verify = %+v, want ran+passed with command", vr)
		}
	})

	t.Run("failed", func(t *testing.T) {
		_, vr := splitVerifyTrailer(`<verify>{"ran":true,"passed":false,"command":"go test ./..."}</verify>`)
		if vr == nil || !vr.Ran || vr.Passed {
			t.Errorf("verify = %+v, want ran && !passed", vr)
		}
	})

	t.Run("skipped (ran=false)", func(t *testing.T) {
		_, vr := splitVerifyTrailer(`<verify>{"ran":false,"skipped_reason":"docs-only"}</verify>`)
		if vr == nil || vr.Ran || vr.SkippedReason != "docs-only" {
			t.Errorf("verify = %+v, want ran=false with skipped_reason", vr)
		}
	})

	t.Run("absent → nil, passthrough", func(t *testing.T) {
		summary, vr := splitVerifyTrailer("no tag here")
		if summary != "no tag here" || vr != nil {
			t.Errorf("got (%q, %+v), want unchanged summary + nil", summary, vr)
		}
	})

	t.Run("malformed JSON → nil but tag stripped", func(t *testing.T) {
		summary, vr := splitVerifyTrailer("body <verify>not json</verify>")
		if vr != nil {
			t.Errorf("verify = %+v, want nil on unparseable JSON", vr)
		}
		if summary != "body" {
			t.Errorf("summary = %q, want %q", summary, "body")
		}
	})
}

// A failing verify must carry the failure OUTPUT, not just the command that
// produced it. Without this the runner reports "agent self-verification
// failed: <command>" and nothing else — naming the ritual, not the cause —
// and whoever is debugging has to reproduce the failure themselves to learn
// what it was.
func TestSplitVerifyTrailerCarriesFailureOutput(t *testing.T) {
	summary, vr := splitVerifyTrailer("Tried it.\n\n" +
		`<verify>{"ran":true,"passed":false,"command":"go test ./...","stderr_tail":"pkg/auth/token.go:42:9: undefined: ParseJWT"}</verify>`)
	if vr == nil {
		t.Fatal("no verify result parsed")
	}
	if vr.Passed {
		t.Error("passed must be false")
	}
	if vr.StderrTail != "pkg/auth/token.go:42:9: undefined: ParseJWT" {
		t.Errorf("StderrTail = %q — the verbatim failure text is the whole point", vr.StderrTail)
	}
	if summary != "Tried it." {
		t.Errorf("summary = %q, want the trailer stripped", summary)
	}
}

// The instruction has to actually ask for it, or the field stays empty
// forever — which is exactly what it did before this: result.VerifyResult
// has carried StdoutTail/StderrTail all along, and nothing ever populated
// them because the prompt never mentioned them.
func TestFinalMessageInstructionAsksForFailureOutput(t *testing.T) {
	if !strings.Contains(finalMessageInstruction, "stderr_tail") {
		t.Error("the prompt must ask for stderr_tail, or the field is never populated")
	}
	if !strings.Contains(finalMessageInstruction, "verbatim") {
		t.Error("the prompt must ask for verbatim output — a paraphrased error is the " +
			"one thing the reader could already guess")
	}
	// Asked for only on failure, so the common path costs no extra tokens
	// in a prompt that runs on every call.
	if !strings.Contains(finalMessageInstruction, "When passed is false") {
		t.Error("the tail must be scoped to the failing case")
	}
}
