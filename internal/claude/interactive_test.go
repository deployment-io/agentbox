package claude

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/agent"
	"github.com/deployment-io/agentbox/internal/config"
)

func argValue(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func hasArg(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func TestBuildInteractiveArgs_ReadOnly(t *testing.T) {
	d := &Driver{}
	cfg := &config.Config{
		Mode:               config.ModeInteractive,
		ReadOnly:           true,
		SessionID:          "sess-123",
		AppendSystemPrompt: "be read only",
		Model:              "claude-sonnet-4-6",
	}
	args := d.BuildInteractiveArgs(cfg)

	if v, _ := argValue(args, "--input-format"); v != "stream-json" {
		t.Errorf("--input-format = %q, want stream-json", v)
	}
	if v, _ := argValue(args, "--output-format"); v != "stream-json" {
		t.Errorf("--output-format = %q, want stream-json", v)
	}
	if !hasArg(args, "--include-partial-messages") {
		t.Error("missing --include-partial-messages")
	}
	if v, _ := argValue(args, "--session-id"); v != "sess-123" {
		t.Errorf("--session-id = %q", v)
	}
	if v, _ := argValue(args, "--append-system-prompt"); v != "be read only" {
		t.Errorf("--append-system-prompt = %q", v)
	}
	if v, _ := argValue(args, "--model"); v != "claude-sonnet-4-6" {
		t.Errorf("--model = %q", v)
	}
	if !hasArg(args, "--allowedTools") {
		t.Error("read-only must pass --allowedTools")
	}
	// The security-critical assertion: read-only must NOT skip
	// permissions, or the allowlist is bypassed.
	if hasArg(args, "--dangerously-skip-permissions") {
		t.Error("read-only must NOT skip permissions (it would bypass the allowlist)")
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "Bash(git log *)") {
		t.Errorf("allowlist should include git log: %s", joined)
	}
	if !strings.Contains(joined, "Read") {
		t.Error("allowlist should include Read")
	}
}

func TestBuildInteractiveArgs_NotReadOnly(t *testing.T) {
	d := &Driver{}
	cfg := &config.Config{Mode: config.ModeInteractive, ReadOnly: false}
	args := d.BuildInteractiveArgs(cfg)
	if !hasArg(args, "--dangerously-skip-permissions") {
		t.Error("non-read-only interactive should skip permission prompts")
	}
	if hasArg(args, "--allowedTools") {
		t.Error("non-read-only should not pass an allowlist")
	}
}

func TestBuildInteractiveArgs_OptionalFlags(t *testing.T) {
	d := &Driver{}
	args := d.BuildInteractiveArgs(&config.Config{Mode: config.ModeInteractive})
	if hasArg(args, "--max-budget-usd") {
		t.Error("budget flag should be absent when unset")
	}
	if hasArg(args, "--session-id") {
		t.Error("session flag should be absent when unset")
	}
	if hasArg(args, "--model") {
		t.Error("model flag should be absent when unset")
	}

	args = d.BuildInteractiveArgs(&config.Config{Mode: config.ModeInteractive, MaxBudgetUSD: "5.00"})
	if v, _ := argValue(args, "--max-budget-usd"); v != "5.00" {
		t.Errorf("--max-budget-usd = %q, want 5.00", v)
	}
}

func TestEncodeUserMessage(t *testing.T) {
	d := &Driver{}
	in := `fix the "login" bug` + "\nsecond line"
	line, err := d.encodeUserMessage(agent.UserMessage{ID: "1", Text: in})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(line) == 0 || line[len(line)-1] != '\n' {
		t.Fatal("envelope must end with a newline")
	}
	// Exactly one line (the embedded newline in the user text must be
	// escaped, not break the envelope into two lines).
	if strings.Count(strings.TrimRight(string(line), "\n"), "\n") != 0 {
		t.Errorf("envelope should be a single JSON line, got: %q", line)
	}

	var got struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatalf("envelope is not valid JSON: %v", err)
	}
	if got.Type != "user" {
		t.Errorf("type = %q, want user", got.Type)
	}
	if got.Message.Role != "user" {
		t.Errorf("role = %q, want user", got.Message.Role)
	}
	if got.Message.Content != in {
		t.Errorf("content round-trip mismatch: got %q want %q", got.Message.Content, in)
	}
}

// A turn with no images must produce exactly the envelope that shipped before
// images existed — pinned literally, because a stray "content":[] would change
// how every existing session is encoded.
func TestEncodeUserMessage_NoImagesIsUnchanged(t *testing.T) {
	d := &Driver{}
	line, err := d.encodeUserMessage(agent.UserMessage{ID: "1", Text: "hello"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	const want = `{"type":"user","message":{"role":"user","content":"hello"}}` + "\n"
	if string(line) != want {
		t.Errorf("envelope = %q, want %q", line, want)
	}
}

// userTurn is the decoded envelope for a turn that carries images: content is
// an array of blocks rather than a string.
type userTurn struct {
	Type    string `json:"type"`
	Message struct {
		Role    string `json:"role"`
		Content []struct {
			Type   string `json:"type"`
			Text   string `json:"text"`
			Source struct {
				Type      string `json:"type"`
				MediaType string `json:"media_type"`
				Data      string `json:"data"`
			} `json:"source"`
		} `json:"content"`
	} `json:"message"`
}

func TestEncodeUserMessage_ImagesBecomeContentBlocks(t *testing.T) {
	dir := t.TempDir()
	shot := filepath.Join(dir, "shot.png")
	photo := filepath.Join(dir, "photo.jpg")
	if err := os.WriteFile(shot, []byte("\x89PNG-pixels"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(photo, []byte("\xff\xd8jpeg-pixels"), 0o644); err != nil {
		t.Fatal(err)
	}

	d := &Driver{}
	line, err := d.encodeUserMessage(agent.UserMessage{ID: "1", Text: "what's wrong here?", Images: []agent.UserImage{
		{Path: shot, MediaType: "image/png", Width: 1280, Height: 800},
		{Path: photo, MediaType: "image/jpeg", Width: 4, Height: 3},
	}})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if strings.Count(strings.TrimRight(string(line), "\n"), "\n") != 0 {
		t.Errorf("envelope must stay a single line: %q", line)
	}
	var got userTurn
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatalf("envelope is not valid JSON: %v", err)
	}
	if got.Type != "user" || got.Message.Role != "user" {
		t.Errorf("envelope = %+v", got)
	}
	if len(got.Message.Content) != 3 {
		t.Fatalf("content blocks = %d, want 3", len(got.Message.Content))
	}
	// The text comes first, verbatim.
	if got.Message.Content[0].Type != "text" || got.Message.Content[0].Text != "what's wrong here?" {
		t.Errorf("first block = %+v", got.Message.Content[0])
	}
	for i, want := range []struct {
		mediaType string
		data      string
	}{
		{"image/png", base64.StdEncoding.EncodeToString([]byte("\x89PNG-pixels"))},
		{"image/jpeg", base64.StdEncoding.EncodeToString([]byte("\xff\xd8jpeg-pixels"))},
	} {
		b := got.Message.Content[i+1]
		if b.Type != "image" || b.Source.Type != "base64" || b.Source.MediaType != want.mediaType || b.Source.Data != want.data {
			t.Errorf("image block %d = %+v, want %s / %s", i, b, want.mediaType, want.data)
		}
	}
}

// An image the agent can't read must not cost the user their message.
func TestEncodeUserMessage_UnreadableImageStillDeliversTheTurn(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "ok.png")
	if err := os.WriteFile(ok, []byte("pixels"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "gone.png")

	d := &Driver{}
	line, err := d.encodeUserMessage(agent.UserMessage{ID: "1", Text: "look", Images: []agent.UserImage{
		{Path: missing, MediaType: "image/png"},
		{Path: ok, MediaType: "image/png"},
	}})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got userTurn
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatalf("envelope is not valid JSON: %v", err)
	}
	if len(got.Message.Content) != 2 {
		t.Fatalf("content blocks = %d, want the text plus the one readable image", len(got.Message.Content))
	}
	text := got.Message.Content[0].Text
	if !strings.HasPrefix(text, "look") || !strings.Contains(text, "could not be loaded") || !strings.Contains(text, missing) {
		t.Errorf("text block = %q, want the message plus a note naming %s", text, missing)
	}
	if got.Message.Content[1].Type != "image" {
		t.Errorf("the readable image must still be attached: %+v", got.Message.Content[1])
	}
}
