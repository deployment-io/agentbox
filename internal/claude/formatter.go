package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode/utf8"
)

// Per-event line-width budget. The dashboard's log viewer renders in a
// fixed-width monospace font; lines beyond ~200 chars wrap awkwardly.
// Aggressive truncation keeps the viewport readable — full payloads
// remain available in the raw stream-json on /scratch/agent.log.
const (
	maxLineWidth        = 200
	thinkingExcerptLen  = 120
	textExcerptLen      = 160
	toolInputExcerptLen = 140
	resultExcerptLen    = 200
)

// humanLogFormatter wraps a sink io.Writer with a line-buffered
// translator that turns Claude Code's stream-json into compact summary
// lines (`[init] ...`, `[thinking] ...`, `[tool] ...`, `[result] ...`,
// `[text] ...`, `[done] ...`). Bytes that don't parse as stream-json
// (npm install output, proxy deny logs, Node stack traces) pass through
// verbatim — they're already human-readable and often the important
// part of an incident.
//
// rawSink, when non-nil, receives every byte before line-splitting.
// Typical caller wires it to /scratch/agent.log so the unfiltered
// stream-json stays available for deep debugging.
//
// Concurrency: cmd.Stdout is driven from a single goroutine spun up by
// os/exec, so Write() is effectively single-threaded in practice. The
// mutex guards Close() against a late Write that the runtime might
// still flush, and keeps the buffer consistent if the writer is ever
// shared.
type humanLogFormatter struct {
	sink    io.Writer
	rawSink io.WriteCloser

	mu  sync.Mutex
	buf bytes.Buffer
}

func newHumanLogFormatter(sink io.Writer, rawSink io.WriteCloser) *humanLogFormatter {
	return &humanLogFormatter{sink: sink, rawSink: rawSink}
}

// Write buffers incoming bytes, splits on newline, and emits one
// summary line per logical event. Errors writing to sink are swallowed
// — the prior MultiWriter→os.Stdout path had the same posture, and a
// failed log write must not stall the agent run.
func (f *humanLogFormatter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.rawSink != nil {
		_, _ = f.rawSink.Write(p)
	}

	f.buf.Write(p)
	for {
		b := f.buf.Bytes()
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			break
		}
		line := append([]byte(nil), b[:i]...)
		f.buf.Next(i + 1)
		f.emitLine(line)
	}
	return len(p), nil
}

// Close flushes any pending partial line, then closes rawSink if owned.
// Safe to call multiple times.
func (f *humanLogFormatter) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.buf.Len() > 0 {
		line := append([]byte(nil), f.buf.Bytes()...)
		f.buf.Reset()
		f.emitLine(line)
	}
	var err error
	if f.rawSink != nil {
		err = f.rawSink.Close()
		f.rawSink = nil
	}
	return err
}

func (f *humanLogFormatter) emitLine(line []byte) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return
	}
	// Cheap early-out: lines that don't start with '{' aren't stream-
	// json events. Forwarding the raw line costs nothing and preserves
	// proxy/npm/Node output that's already legible.
	if trimmed[0] != '{' {
		f.passthrough(line)
		return
	}
	summary, ok := formatStreamJSONLine(trimmed)
	if !ok {
		f.passthrough(line)
		return
	}
	if summary == "" {
		return
	}
	for _, l := range strings.Split(summary, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		_, _ = io.WriteString(f.sink, truncate(l, maxLineWidth))
		_, _ = f.sink.Write([]byte{'\n'})
	}
}

func (f *humanLogFormatter) passthrough(line []byte) {
	_, _ = f.sink.Write(line)
	_, _ = f.sink.Write([]byte{'\n'})
}

// formatStreamJSONLine parses one stream-json line and returns its
// human summary plus ok=true. ok=false signals "not a stream-json event
// I recognize — pass through raw." Empty string with ok=true means
// the event was recognized but had no useful content to surface.
func formatStreamJSONLine(line []byte) (string, bool) {
	var ev humanEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return "", false
	}
	if ev.Type == "" {
		return "", false
	}
	return ev.format(), true
}

// humanEvent is the formatter's view of a stream-json event. Distinct
// from streamEvent in parser.go: we need richer fields here (message
// content blocks, duration, tools list, cwd) but don't share the
// parser's state. Both parse the same line independently — cheap, and
// avoids coupling the formatter to the parser's lock.
type humanEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`

	// system.init fields
	Model      string   `json:"model"`
	Tools      []string `json:"tools"`
	MCPServers []any    `json:"mcp_servers"`
	Cwd        string   `json:"cwd"`

	// assistant / user envelope
	Message json.RawMessage `json:"message"`

	// result event fields
	Result     string      `json:"result"`
	NumTurns   int         `json:"num_turns"`
	DurationMs int64       `json:"duration_ms"`
	IsError    bool        `json:"is_error"`
	Usage      *humanUsage `json:"usage"`
}

type humanUsage struct {
	InputTokens             int `json:"input_tokens"`
	OutputTokens            int `json:"output_tokens"`
	CacheReadInputTokens    int `json:"cache_read_input_tokens"`
	CacheCreationInputToken int `json:"cache_creation_input_tokens"`
}

type humanMessage struct {
	Role    string              `json:"role"`
	Model   string              `json:"model"`
	Content []humanContentBlock `json:"content"`
}

type humanContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	Content   json.RawMessage `json:"content"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
}

func (e humanEvent) format() string {
	switch e.Type {
	case "system":
		return formatSystem(e)
	case "assistant", "user":
		return formatMessageEnvelope(e.Message)
	case "result":
		return formatResult(e)
	default:
		// Unknown event types pass through with a minimal tag so
		// information isn't silently dropped — over-logging beats
		// under-logging per the design constraints.
		return fmt.Sprintf("[%s]", e.Type)
	}
}

func formatSystem(e humanEvent) string {
	if e.Subtype != "init" {
		if e.Subtype != "" {
			return "[system] " + e.Subtype
		}
		return "[system]"
	}
	parts := []string{"[init]"}
	if e.Model != "" {
		parts = append(parts, "model="+e.Model)
	}
	if e.Tools != nil {
		parts = append(parts, fmt.Sprintf("tools=%d", len(e.Tools)))
	}
	if e.MCPServers != nil {
		parts = append(parts, fmt.Sprintf("mcp_servers=%d", len(e.MCPServers)))
	}
	if e.Cwd != "" {
		parts = append(parts, "cwd="+e.Cwd)
	}
	return strings.Join(parts, " ")
}

func formatMessageEnvelope(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var msg humanMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return ""
	}
	var lines []string
	for _, b := range msg.Content {
		if line := formatContentBlock(b); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

func formatContentBlock(b humanContentBlock) string {
	switch b.Type {
	case "thinking":
		text := strings.TrimSpace(b.Thinking)
		if text == "" {
			// Pure-signature thinking blocks contain only the
			// encryption blob with no visible text — drop the whole
			// line rather than emit a content-free "[thinking]".
			return ""
		}
		return "[thinking] " + ellipsisOneLine(text, thinkingExcerptLen)
	case "text":
		cleaned, _ := splitPRTitleTrailer(b.Text)
		text := strings.TrimSpace(cleaned)
		if text == "" {
			return ""
		}
		return "[text] " + ellipsisOneLine(text, textExcerptLen)
	case "tool_use":
		return formatToolUse(b)
	case "tool_result":
		return formatToolResult(b)
	default:
		return ""
	}
}

func formatToolUse(b humanContentBlock) string {
	name := b.Name
	if name == "" {
		name = "?"
	}
	summary := summarizeToolInput(name, b.Input)
	if summary == "" {
		return "[tool] " + name
	}
	return "[tool] " + name + ": " + ellipsisOneLine(summary, toolInputExcerptLen)
}

func formatToolResult(b humanContentBlock) string {
	body := toolResultBody(b.Content)
	status := "success"
	if b.IsError {
		status = "error"
	}
	prefix := fmt.Sprintf("[result] (%s, %d bytes)", status, len(body))
	excerpt := strings.TrimSpace(body)
	if excerpt == "" {
		return prefix
	}
	return prefix + " " + ellipsisOneLine(excerpt, resultExcerptLen)
}

// toolResultBody handles the polymorphic `content` field of tool_result
// blocks. The field is either a plain string ("ls output...") or an
// array of text/image blocks. We concatenate text and ignore images.
func toolResultBody(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []humanContentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var sb strings.Builder
		for _, b := range blocks {
			if b.Text == "" {
				continue
			}
			if sb.Len() > 0 {
				sb.WriteByte(' ')
			}
			sb.WriteString(b.Text)
		}
		return sb.String()
	}
	return ""
}

// summarizeToolInput maps a tool's input JSON to a short identifying
// string. Known tools get a hand-picked field (Bash → command, Edit →
// file_path, etc.). Unknown tools fall back to whichever of a few
// common identifying fields is set, so adding a new tool to Claude
// Code's catalog doesn't make agentbox blind to it.
func summarizeToolInput(name string, raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(raw, &generic); err != nil {
		return strings.TrimSpace(string(raw))
	}
	switch name {
	case "Bash":
		if cmd := stringField(generic, "command"); cmd != "" {
			return firstLine(cmd)
		}
		return stringField(generic, "description")
	case "Edit", "MultiEdit", "NotebookEdit", "Write", "Read":
		return stringField(generic, "file_path")
	case "Grep":
		pat := stringField(generic, "pattern")
		if path := stringField(generic, "path"); path != "" {
			return fmt.Sprintf("%s in %s", pat, path)
		}
		return pat
	case "Glob":
		return stringField(generic, "pattern")
	case "WebFetch":
		return stringField(generic, "url")
	case "WebSearch":
		return stringField(generic, "query")
	case "TodoWrite":
		var t struct {
			Todos []json.RawMessage `json:"todos"`
		}
		if err := json.Unmarshal(raw, &t); err == nil {
			return fmt.Sprintf("%d items", len(t.Todos))
		}
		return ""
	case "Task":
		if d := stringField(generic, "description"); d != "" {
			return d
		}
		return stringField(generic, "subagent_type")
	}
	for _, key := range []string{"command", "file_path", "url", "query", "pattern", "description", "name"} {
		if v := stringField(generic, key); v != "" {
			return v
		}
	}
	return ""
}

func stringField(m map[string]json.RawMessage, key string) string {
	raw, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

func formatResult(e humanEvent) string {
	status := "ok"
	if e.IsError {
		status = "error"
	}
	parts := []string{"[done]", "status=" + status}
	// Surface the agent's own failure subtype (e.g. error_max_turns) so
	// the one-line summary says *why* it failed, not just that it did.
	if e.IsError && e.Subtype != "" && e.Subtype != "success" {
		parts = append(parts, "reason="+e.Subtype)
	}
	if e.NumTurns > 0 {
		parts = append(parts, fmt.Sprintf("turns=%d", e.NumTurns))
	}
	if e.Usage != nil {
		parts = append(parts, fmt.Sprintf("tokens=%s/%s",
			humanTokens(e.Usage.InputTokens),
			humanTokens(e.Usage.OutputTokens)))
	}
	if e.DurationMs > 0 {
		parts = append(parts, "duration="+durationHuman(e.DurationMs))
	}
	line := strings.Join(parts, " ")
	cleaned, _ := splitPRTitleTrailer(e.Result)
	if summary := strings.TrimSpace(cleaned); summary != "" {
		line += " summary=" + ellipsisOneLine(summary, resultExcerptLen)
	}
	return line
}

func humanTokens(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
}

func durationHuman(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	sec := float64(ms) / 1000
	if sec < 60 {
		return fmt.Sprintf("%.1fs", sec)
	}
	m := int(sec) / 60
	return fmt.Sprintf("%dm%02ds", m, int(sec)%60)
}

func ellipsisOneLine(s string, max int) string {
	s = collapseWhitespace(s)
	return truncate(s, max)
}

func collapseWhitespace(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		switch r {
		case '\n', '\r', '\t':
			r = ' '
		}
		if r == ' ' {
			if prevSpace {
				continue
			}
			prevSpace = true
		} else {
			prevSpace = false
		}
		sb.WriteRune(r)
	}
	return strings.TrimSpace(sb.String())
}

func truncate(s string, max int) string {
	if max <= 0 {
		return s
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max]) + "…"
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}
