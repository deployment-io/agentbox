package claude

import "testing"

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
