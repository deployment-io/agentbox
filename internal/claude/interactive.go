package claude

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/deployment-io/agentbox/internal/agent"
	"github.com/deployment-io/agentbox/internal/config"
)

// readOnlyAllowedTools is the tool + Bash sub-command allowlist for
// read-only investigation sessions AND for review runs. In headless (-p)
// mode WITHOUT --dangerously-skip-permissions, a tool call that matches
// nothing here is denied outright (there is no human to prompt), which is
// exactly the read-only behavior we want. The space before `*` enforces a
// word boundary, so "Bash(git log *)" matches `git log --oneline` but not a
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

// encodeUserMessage renders one user turn as the stream-json stdin envelope
// Claude Code reads under --input-format stream-json, with a trailing
// newline so it is a complete line. Shape:
//
//	{"type":"user","message":{"role":"user","content":"..."}}
//
// A turn carrying images uses the Messages API's other content shape — an
// array of blocks, the text first, then one base64 image block each:
//
//	{"type":"user","message":{"role":"user","content":[
//	  {"type":"text","text":"..."},
//	  {"type":"image","source":{"type":"base64","media_type":"image/png","data":"..."}}]}}
//
// A turn with no images encodes as the plain string, byte-for-byte as it did
// before images existed. json.Marshal escapes the user text, so arbitrary
// content (quotes, newlines, control characters) is safe to embed. Passed to
// agent.PumpTextStdin by RunSession.
func (d *Driver) encodeUserMessage(msg agent.UserMessage) ([]byte, error) {
	var env userEnvelope
	env.Type = "user"
	env.Message.Role = "user"
	if len(msg.Images) == 0 {
		env.Message.Content = msg.Text
	} else {
		blocks, note := imageBlocks(msg.Images)
		text := msg.Text
		if note != "" {
			text += "\n\n" + note
		}
		env.Message.Content = append([]userContentBlock{{Type: "text", Text: text}}, blocks...)
	}
	b, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("encode user message: %w", err)
	}
	return append(b, '\n'), nil
}

// imageBlocks reads each image off disk and renders it as a base64 content
// block. An unreadable image is reported to the user as a note appended to the
// turn's text rather than failing the turn: the message itself is what the user
// is waiting on, and an agent told an image is missing can ask for it again —
// an agent that never gets the turn cannot.
func imageBlocks(images []agent.UserImage) (blocks []userContentBlock, note string) {
	var missing []string
	for _, img := range images {
		data, err := os.ReadFile(img.Path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[agentbox] cannot read attached image %s: %v\n", img.Path, err)
			missing = append(missing, img.Path)
			continue
		}
		blocks = append(blocks, userContentBlock{
			Type: "image",
			Source: &userImageSource{
				Type:      "base64",
				MediaType: img.MediaType,
				Data:      base64.StdEncoding.EncodeToString(data),
			},
		})
	}
	if len(missing) > 0 {
		note = "(note: an attached image could not be loaded: " + strings.Join(missing, ", ") + ")"
	}
	return blocks, note
}

// userEnvelope's Content is `any` so one struct covers both content shapes the
// Messages API accepts: a plain string, or an array of blocks.
type userEnvelope struct {
	Type    string `json:"type"`
	Message struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	} `json:"message"`
}

type userContentBlock struct {
	Type   string           `json:"type"`
	Text   string           `json:"text,omitempty"`
	Source *userImageSource `json:"source,omitempty"`
}

type userImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}
