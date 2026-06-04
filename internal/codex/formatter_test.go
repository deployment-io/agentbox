package codex

import (
	"bytes"
	"strings"
	"testing"
)

// format runs a single JSONL line through the formatter and returns the
// human output (the partial-line case is covered separately).
func format(t *testing.T, line string) string {
	t.Helper()
	var sink bytes.Buffer
	f := newHumanLogFormatter(&sink, nil)
	if _, err := f.Write([]byte(line + "\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return sink.String()
}

func TestFormatCodexJSONLine_Events(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string // substring expected; "" means the event is dropped
	}{
		{"session start", `{"type":"thread.started","thread_id":"t1"}`, "codex session started"},
		{"turn started dropped", `{"type":"turn.started"}`, ""},
		{
			"turn completed with usage",
			`{"type":"turn.completed","usage":{"input_tokens":100,"output_tokens":20,"cached_input_tokens":80}}`,
			"turn complete",
		},
		{
			"command start shows command",
			`{"type":"item.started","item":{"type":"command_execution","command":"go test ./..."}}`,
			"$ go test ./...",
		},
		{
			"command clean exit dropped",
			`{"type":"item.completed","item":{"type":"command_execution","command":"go test","exit_code":0}}`,
			"",
		},
		{
			"command failed exit shown",
			`{"type":"item.completed","item":{"type":"command_execution","command":"go test","exit_code":1}}`,
			"exit 1",
		},
		{
			"file change lists path",
			`{"type":"item.completed","item":{"type":"file_change","changes":[{"path":"main.go","kind":"update"}]}}`,
			"main.go",
		},
		{
			"agent message on completion",
			`{"type":"item.completed","item":{"type":"agent_message","text":"Added the /health route."}}`,
			"Added the /health route.",
		},
		{
			"agent message start dropped",
			`{"type":"item.started","item":{"type":"agent_message","text":"thinking"}}`,
			"",
		},
		{"turn failed surfaces error", `{"type":"turn.failed","error":{"message":"rate limit reached"}}`, "rate limit reached"},
		{"error event flat message", `{"type":"error","message":"context canceled"}`, "context canceled"},
		{"non-json passthrough", `npm warn deprecated foo@1.0.0`, "npm warn deprecated foo@1.0.0"},
		{"unknown event dropped", `{"type":"some.future.event"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := format(t, tc.line)
			if tc.want == "" {
				if strings.TrimSpace(got) != "" {
					t.Errorf("expected no output, got %q", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("output %q does not contain %q", got, tc.want)
			}
		})
	}
}

// A multi-file change should list every path with its diff-style glyph.
func TestFormatCodexItem_MultipleFileChanges(t *testing.T) {
	line := `{"type":"item.completed","item":{"type":"file_change","changes":[` +
		`{"path":"a.go","kind":"add"},{"path":"b.go","kind":"delete"},{"path":"c.go","kind":"update"}]}}`
	got := format(t, line)
	for _, want := range []string{"+ a.go", "- b.go", "~ c.go"} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q missing %q", got, want)
		}
	}
}

// A logical event split across two Write calls must be buffered and emitted
// exactly once, on completion.
func TestFormatter_LineBufferingAcrossWrites(t *testing.T) {
	var sink bytes.Buffer
	f := newHumanLogFormatter(&sink, nil)
	if _, err := f.Write([]byte(`{"type":"thread.started"`)); err != nil {
		t.Fatal(err)
	}
	if sink.Len() != 0 {
		t.Errorf("partial line should not emit yet, got %q", sink.String())
	}
	if _, err := f.Write([]byte("}\n")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sink.String(), "codex session started") {
		t.Errorf("completed line should emit, got %q", sink.String())
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// Close must flush a trailing line that never received its newline.
func TestFormatter_CloseFlushesPartial(t *testing.T) {
	var sink bytes.Buffer
	f := newHumanLogFormatter(&sink, nil)
	if _, err := f.Write([]byte(`{"type":"thread.started"}`)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sink.String(), "codex session started") {
		t.Errorf("Close should flush the partial line, got %q", sink.String())
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("short string should be unchanged, got %q", got)
	}
	got := truncate("hello world", 5)
	if []rune(got)[len([]rune(got))-1] != '…' {
		t.Errorf("truncated string should end with ellipsis, got %q", got)
	}
	if n := len([]rune(got)); n != 5 {
		t.Errorf("truncated rune length = %d, want 5", n)
	}
}
