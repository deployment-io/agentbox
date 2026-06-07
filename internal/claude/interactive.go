package claude

import (
	"encoding/json"
	"fmt"

	"github.com/deployment-io/agentbox/internal/config"
)

// readOnlyAllowedTools is the tool + Bash sub-command allowlist for
// read-only investigation sessions. In headless (-p) mode WITHOUT
// --dangerously-skip-permissions, a tool call that matches nothing here
// is denied outright (there is no human to prompt), which is exactly the
// read-only behavior we want. The space before `*` enforces a word
// boundary, so "Bash(git log *)" matches `git log --oneline` but not a
// command merely starting with "git log"-something.
//
// This is the safety floor; the system prompt is the policy ceiling.
// Extend deliberately — every entry must be side-effect free.
var readOnlyAllowedTools = []string{
	"Read",
	"Grep",
	"Glob",
	"Bash(rg *)",
	"Bash(grep *)",
	"Bash(git log *)",
	"Bash(git diff *)",
	"Bash(git show *)",
	"Bash(git status *)",
	"Bash(git blame *)",
	"Bash(ls *)",
	"Bash(cat *)",
	"Bash(head *)",
	"Bash(tail *)",
	"Bash(find *)",
	"Bash(wc *)",
	"Bash(tree *)",
}

// BuildInteractiveArgs builds the argv for a long-lived bidirectional
// session: user turns arrive as line-delimited stream-json on stdin and
// the agent emits its events (including token-level partials) as
// stream-json on stdout. --verbose is required for stream-json with -p.
func (d *Driver) BuildInteractiveArgs(cfg *config.Config) []string {
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--include-partial-messages",
		"--verbose",
	}
	if cfg.SessionID != "" {
		args = append(args, "--session-id", cfg.SessionID)
	}
	if cfg.AppendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", cfg.AppendSystemPrompt)
	}
	if cfg.MaxBudgetUSD != "" {
		args = append(args, "--max-budget-usd", cfg.MaxBudgetUSD)
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.ReadOnly {
		// Whitelist only; deliberately NO --dangerously-skip-permissions,
		// which would bypass the allowlist and defeat read-only safety.
		// Appended last: --allowedTools is variadic, so it consumes every
		// following token until end-of-args. Each entry is its own argv
		// token, so a pattern containing spaces ("Bash(git log *)") stays
		// atomic — no shell splitting.
		args = append(args, "--allowedTools")
		args = append(args, readOnlyAllowedTools...)
	} else {
		// Non-read-only interactive sessions still skip the interactive
		// permission prompts — there is no human at the CLI to answer them.
		args = append(args, "--dangerously-skip-permissions")
	}
	return args
}

// EncodeUserMessage renders one user turn as the stream-json stdin
// envelope Claude Code reads under --input-format stream-json, with a
// trailing newline so it is a complete line. Shape:
//
//	{"type":"user","message":{"role":"user","content":"..."}}
//
// json.Marshal escapes the user text, so arbitrary content (quotes,
// newlines, control characters) is safe to embed.
func (d *Driver) EncodeUserMessage(text string) ([]byte, error) {
	var env userEnvelope
	env.Type = "user"
	env.Message.Role = "user"
	env.Message.Content = text
	b, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("encode user message: %w", err)
	}
	return append(b, '\n'), nil
}

type userEnvelope struct {
	Type    string `json:"type"`
	Message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
}
