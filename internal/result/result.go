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

// agentType is "claude-code" for v1. v2+ will dispatch on AGENT_TYPE.
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

// Outcome is the structured result of an agent run.
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

	// ExitCode is returned by the process; not part of the JSON shape.
	ExitCode int `json:"-"`
}

type TokenUsage struct {
	InputTokens     int `json:"input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	CacheReadTokens int `json:"cache_read_tokens"`
}

// Path returns the destination for result.json — $RESULT_PATH or the
// default. See docs/CONTRACT.md for bind-mount guidance.
func Path() string {
	if p := os.Getenv("RESULT_PATH"); p != "" {
		return p
	}
	return "/tmp/result.json"
}

// Write serializes the Outcome as JSON. SchemaVersion and AgentType are
// overwritten; AgentVersion is passed through (caller sets it).
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

// WriteFailure writes an Outcome for a pre-execution failure where the
// agent never ran. Summary is optional.
func WriteFailure(err error, summary string) error {
	return Write(Outcome{
		Status:         StatusFailure,
		ChangesSummary: summary,
		Error:          err.Error(),
		ExitCode:       ExitExecutionFailure,
	})
}
