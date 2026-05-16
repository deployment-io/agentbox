package claude

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// representativeStream is a hand-crafted fixture meant to cover every
// event shape the formatter is expected to handle, including:
//   - system.init with a tools list and mcp_servers
//   - an assistant message carrying signature-only thinking
//   - an assistant message with thinking text, an analysis text block,
//     and a Bash tool_use
//   - a user message bundling a tool_result
//   - an Edit / Write / Read / Grep / TodoWrite tool_use (varied tools)
//   - a final result event with usage stats and a duration
//   - a non-JSON line (proxy deny) that should pass through verbatim
//
// Each line is one stream-json event terminated by '\n', mirroring the
// shape Claude Code actually emits.
const representativeStream = `{"type":"system","subtype":"init","session_id":"abc","model":"claude-opus-4-7","tools":["Bash","Edit","Read","Write","Grep","Glob","WebFetch","TodoWrite"],"mcp_servers":[],"permissionMode":"bypassPermissions","cwd":"/work"}
{"type":"assistant","session_id":"abc","uuid":"u1","parent_tool_use_id":null,"message":{"id":"msg_01","model":"claude-opus-4-7","role":"assistant","content":[{"type":"thinking","thinking":"","signature":"EvgBCkID..."}]}}
{"type":"assistant","session_id":"abc","uuid":"u2","message":{"id":"msg_02","model":"claude-opus-4-7","role":"assistant","content":[{"type":"thinking","thinking":"I need to read the file, then look for the function definition.\nLet me start by listing the directory.","signature":"EvgB..."},{"type":"text","text":"Let me explore the codebase first."},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls -la /work","description":"List work dir"}}]}}
{"type":"user","session_id":"abc","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"total 16\ndrwxr-xr-x 2 agent agent 4096 Mar  1 12:00 .\n-rw-r--r-- 1 agent agent  123 Mar  1 12:00 main.go","is_error":false}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/work/main.go"}}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/work/main.go","old_string":"hello","new_string":"world"}}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"func main","path":"/work"}}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"TodoWrite","input":{"todos":[{"content":"a"},{"content":"b"},{"content":"c"}]}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_2","content":[{"type":"text","text":"file not found"}],"is_error":true}]}}
[agentbox] proxy denied CONNECT to evil.example.com
{"type":"result","subtype":"success","is_error":false,"num_turns":4,"duration_ms":23150,"result":"Replaced 'hello' with 'world' in main.go and added a test.","session_id":"abc","total_cost_usd":0.05,"usage":{"input_tokens":1200,"output_tokens":350,"cache_read_input_tokens":8000,"cache_creation_input_tokens":2000}}
`

func TestHumanLogFormatter_FixtureEmitsExpectedLines(t *testing.T) {
	var sink bytes.Buffer
	f := newHumanLogFormatter(&sink, nil)
	if _, err := io.WriteString(f, representativeStream); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	got := sink.String()

	wantSubstrings := []string{
		"[init] model=claude-opus-4-7 tools=8 mcp_servers=0 cwd=/work",
		"[thinking] I need to read the file",
		"[text] Let me explore the codebase first.",
		"[tool] Bash: ls -la /work",
		"[result] (success,",
		"[tool] Read: /work/main.go",
		"[tool] Edit: /work/main.go",
		"[tool] Grep: func main in /work",
		"[tool] TodoWrite: 3 items",
		"[result] (error,",
		"file not found",
		"[agentbox] proxy denied CONNECT to evil.example.com",
		"[done] status=ok turns=4 tokens=1.2k/350 duration=23.1s summary=Replaced 'hello' with 'world'",
	}

	for _, want := range wantSubstrings {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n--- got ---\n%s", want, got)
		}
	}

	// Encryption signatures must never leak through.
	if strings.Contains(got, "EvgB") {
		t.Errorf("thinking signature leaked through:\n%s", got)
	}
	// Empty thinking blocks (signature only) should be dropped entirely.
	if strings.Count(got, "[thinking]") != 1 {
		t.Errorf("expected exactly one [thinking] line, got:\n%s", got)
	}
	// IDs that are never useful to humans must be dropped.
	for _, junk := range []string{"toolu_1", "session_id", "uuid", "parent_tool_use_id", "msg_01"} {
		if strings.Contains(got, junk) {
			t.Errorf("output should not contain debug field %q:\n%s", junk, got)
		}
	}
}

func TestHumanLogFormatter_PassesThroughNonJSON(t *testing.T) {
	var sink bytes.Buffer
	f := newHumanLogFormatter(&sink, nil)
	_, _ = io.WriteString(f, "npm WARN install something\nbroken {json line\nnode: unhandled rejection\n")
	_ = f.Close()

	for _, want := range []string{"npm WARN install something", "broken {json line", "node: unhandled rejection"} {
		if !strings.Contains(sink.String(), want) {
			t.Errorf("missing pass-through %q in:\n%s", want, sink.String())
		}
	}
}

func TestHumanLogFormatter_PartialLineFlushOnClose(t *testing.T) {
	var sink bytes.Buffer
	f := newHumanLogFormatter(&sink, nil)
	_, _ = io.WriteString(f, "this has no terminating newline")
	_ = f.Close()

	if !strings.Contains(sink.String(), "this has no terminating newline") {
		t.Errorf("Close should flush the pending partial line: %q", sink.String())
	}
}

func TestHumanLogFormatter_TeesToRawSink(t *testing.T) {
	var sink, raw bytes.Buffer
	f := newHumanLogFormatter(&sink, nopCloser{&raw})
	input := `{"type":"result","is_error":false,"num_turns":1,"result":"done","duration_ms":100}` + "\n"
	_, _ = io.WriteString(f, input)
	_ = f.Close()

	if raw.String() != input {
		t.Errorf("raw sink should mirror input exactly\n got: %q\nwant: %q", raw.String(), input)
	}
	if !strings.Contains(sink.String(), "[done]") {
		t.Errorf("primary sink should have summary: %q", sink.String())
	}
}

func TestHumanLogFormatter_HandlesChunkedWrites(t *testing.T) {
	// stream-json comes through io.Pipe in arbitrary-sized chunks, so
	// the formatter has to buffer until it sees a newline rather than
	// assuming Write deliveries align with event boundaries.
	var sink bytes.Buffer
	f := newHumanLogFormatter(&sink, nil)
	line := `{"type":"system","subtype":"init","model":"claude-opus-4-7","tools":["X","Y"]}` + "\n"
	for i := 0; i < len(line); i += 7 {
		end := i + 7
		if end > len(line) {
			end = len(line)
		}
		_, _ = io.WriteString(f, line[i:end])
	}
	_ = f.Close()

	if !strings.Contains(sink.String(), "[init] model=claude-opus-4-7 tools=2") {
		t.Errorf("chunked writes lost data: %q", sink.String())
	}
}

func TestHumanLogFormatter_TruncatesLongLines(t *testing.T) {
	var sink bytes.Buffer
	f := newHumanLogFormatter(&sink, nil)
	long := strings.Repeat("x", 500)
	event := `{"type":"assistant","message":{"content":[{"type":"text","text":"` + long + `"}]}}` + "\n"
	_, _ = io.WriteString(f, event)
	_ = f.Close()

	out := strings.TrimSuffix(sink.String(), "\n")
	if len(out) > maxLineWidth+len("…") {
		t.Errorf("emitted line exceeded maxLineWidth (%d): %q", len(out), out)
	}
	if !strings.HasSuffix(out, "…") {
		t.Errorf("truncated line should end with ellipsis: %q", out)
	}
}

func TestHumanLogFormatter_DropsUsageAndIDsAcrossManyMessages(t *testing.T) {
	// Mimics the regression the change is meant to fix: 80%+ of every
	// line was envelope noise. Assert each emitted line is small and
	// information-dense.
	var sink bytes.Buffer
	f := newHumanLogFormatter(&sink, nil)
	noisy := `{"type":"assistant","session_id":"long-session-id-that-should-never-leak","uuid":"long-uuid-that-should-never-leak","parent_tool_use_id":null,"message":{"id":"msg_xx","model":"claude-opus-4-7","role":"assistant","content":[{"type":"tool_use","id":"toolu_xx","name":"Edit","input":{"file_path":"/work/a.go","old_string":"x","new_string":"y"}}],"usage":{"input_tokens":4,"output_tokens":2,"cache_read_input_tokens":100,"cache_creation_input_tokens":50}}}` + "\n"
	for i := 0; i < 5; i++ {
		_, _ = io.WriteString(f, noisy)
	}
	_ = f.Close()

	out := sink.String()
	if strings.Contains(out, "session_id") || strings.Contains(out, "uuid") || strings.Contains(out, "parent_tool_use_id") {
		t.Errorf("noise leaked through:\n%s", out)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if len(line) > maxLineWidth+len("…") {
			t.Errorf("line exceeded budget (%d): %q", len(line), line)
		}
	}
	if strings.Count(out, "[tool] Edit: /work/a.go") != 5 {
		t.Errorf("expected 5 [tool] Edit lines, got:\n%s", out)
	}
}

func TestFormatStreamJSONLine_UnknownTypePassesAsTag(t *testing.T) {
	got, ok := formatStreamJSONLine([]byte(`{"type":"some_new_event","payload":42}`))
	if !ok {
		t.Fatal("recognized type should return ok=true")
	}
	if got != "[some_new_event]" {
		t.Errorf("got %q, want [some_new_event]", got)
	}
}

func TestHumanLogFormatter_BlankAndEmptyLinesIgnored(t *testing.T) {
	var sink bytes.Buffer
	f := newHumanLogFormatter(&sink, nil)
	_, _ = io.WriteString(f, "\n\n   \n\n")
	_ = f.Close()
	if sink.Len() != 0 {
		t.Errorf("blank-only input should produce no output, got %q", sink.String())
	}
}

func TestHumanTokens(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1.0k"},
		{1234, "1.2k"},
		{999_999, "1000.0k"},
		{2_500_000, "2.5M"},
	}
	for _, tc := range cases {
		if got := humanTokens(tc.in); got != tc.want {
			t.Errorf("humanTokens(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDurationHuman(t *testing.T) {
	cases := []struct {
		ms   int64
		want string
	}{
		{42, "42ms"},
		{1500, "1.5s"},
		{23150, "23.1s"},
		{125_000, "2m05s"},
	}
	for _, tc := range cases {
		if got := durationHuman(tc.ms); got != tc.want {
			t.Errorf("durationHuman(%d) = %q, want %q", tc.ms, got, tc.want)
		}
	}
}

// nopCloser turns an io.Writer into io.WriteCloser for raw-sink tests.
type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }
