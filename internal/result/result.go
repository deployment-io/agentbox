// Package result defines the /result.json schema and writing semantics.
//
// See docs/CONTRACT.md for the full output spec.
package result

import (
	"encoding/json"
	"os"
)

// schemaVersion is bumped on breaking changes to the result.json shape.
const schemaVersion = 1

// agentType identifies the agent baked into this agentbox image. v1
// supports only claude-code; v2+ will dispatch on the AGENT_TYPE env
// var and set this accordingly.
const agentType = "claude-code"

// Exit codes per docs/CONTRACT.md.
const (
	ExitSuccess          = 0
	ExitExecutionFailure = 1
	ExitAuthFailure      = 2
	ExitCancelled        = 3
	ExitTimeout          = 4
)

// Status values per docs/CONTRACT.md.
type Status string

const (
	StatusSuccess   Status = "success"
	StatusFailure   Status = "failure"
	StatusCancelled Status = "cancelled"
	StatusTimeout   Status = "timeout"
)

// Outcome is the structured result of an agent run, suitable for writing
// to /result.json and inspecting by consumers.
type Outcome struct {
	SchemaVersion  int        `json:"schema_version"`
	AgentType      string     `json:"agent_type"`
	AgentVersion   string     `json:"agent_version"`
	Status         Status     `json:"status"`
	ChangesSummary string     `json:"changes_summary"`
	FilesChanged   []string   `json:"files_changed"`
	TokenUsage     TokenUsage `json:"token_usage"`
	Turns          int        `json:"turns"`
	Error          string     `json:"error,omitempty"`

	// ExitCode is NOT part of the JSON schema — the process returns it
	// directly. Kept on Outcome for internal plumbing convenience.
	ExitCode int `json:"-"`
}

// TokenUsage reflects Claude Code's reported token counts.
type TokenUsage struct {
	InputTokens     int `json:"input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	CacheReadTokens int `json:"cache_read_tokens"`
}

// Path returns the location where /result.json will be written.
// Controlled by the RESULT_PATH env var; defaults to /tmp/result.json
// (writable on tmpfs; consumers who want the file persisted on the host
// should bind-mount a path and set RESULT_PATH to it).
func Path() string {
	if p := os.Getenv("RESULT_PATH"); p != "" {
		return p
	}
	return "/tmp/result.json"
}

// Write serializes the Outcome to the configured result path as JSON.
// Overrides SchemaVersion and AgentType (binary-level invariants) but
// respects the caller-provided AgentVersion so detection logic can live
// with the caller (see agent.DetectVersion).
func Write(o Outcome) error {
	o.SchemaVersion = schemaVersion
	o.AgentType = agentType
	if o.FilesChanged == nil {
		o.FilesChanged = []string{}
	}

	data, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(Path(), append(data, '\n'), 0o644)
}

// WriteFailure writes an Outcome for a pre-execution failure (e.g., config
// error) where the agent never ran. Summary is optional.
func WriteFailure(err error, summary string) error {
	return Write(Outcome{
		Status:         StatusFailure,
		ChangesSummary: summary,
		Error:          err.Error(),
		ExitCode:       ExitExecutionFailure,
	})
}
