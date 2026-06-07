package claude

import (
	"encoding/json"
	"strings"
	"testing"

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
	line, err := d.EncodeUserMessage(in)
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
