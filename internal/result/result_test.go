package result

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWrite_AlwaysSetsSchemaAndAgentType(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	t.Setenv("RESULT_PATH", path)

	err := Write(Outcome{Status: StatusSuccess, ExitCode: ExitSuccess})
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read result.json: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("result.json is not valid JSON: %v", err)
	}

	if got, _ := parsed["schema_version"].(float64); got != 1 {
		t.Errorf("schema_version = %v, want 1", parsed["schema_version"])
	}
	if got, _ := parsed["agent_type"].(string); got != "claude-code" {
		t.Errorf("agent_type = %v, want claude-code", parsed["agent_type"])
	}
	if got, _ := parsed["status"].(string); got != "success" {
		t.Errorf("status = %v, want success", parsed["status"])
	}
}

// Once the orchestrator started forwarding the configured AGENT_TYPE
// through cfg, Write must pass it through unchanged rather than
// overwriting with a built-in constant — otherwise a future "codex"
// agent's result.json would still report "claude-code".
func TestWrite_AgentTypePassedThroughWhenSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	t.Setenv("RESULT_PATH", path)

	if err := Write(Outcome{
		Status:    StatusSuccess,
		AgentType: "codex",
	}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}

	raw, _ := os.ReadFile(path)
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)

	if got, _ := parsed["agent_type"].(string); got != "codex" {
		t.Errorf("agent_type = %v, want codex", parsed["agent_type"])
	}
}

// New "what actually ran" metadata fields must round-trip — they're
// emitted via omitempty so we also assert presence when set and absence
// when not. The dashboard's defensive untyped access depends on these
// being either valid JSON values or absent, never JSON null.
func TestWrite_RunMetadataRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	t.Setenv("RESULT_PATH", path)

	if err := Write(Outcome{
		Status:       StatusSuccess,
		AgentType:    "claude-code",
		AgentVersion: "2.1.117",
		Model:        "claude-opus-4-7",
		StartedAt:    1731600000,
		EndedAt:      1731600135,
	}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}

	raw, _ := os.ReadFile(path)
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)

	if got, _ := parsed["model"].(string); got != "claude-opus-4-7" {
		t.Errorf("model = %v, want claude-opus-4-7", parsed["model"])
	}
	if got, _ := parsed["agent_version"].(string); got != "2.1.117" {
		t.Errorf("agent_version = %v, want 2.1.117", parsed["agent_version"])
	}
	if got, _ := parsed["started_at"].(float64); int64(got) != 1731600000 {
		t.Errorf("started_at = %v, want 1731600000", parsed["started_at"])
	}
	if got, _ := parsed["ended_at"].(float64); int64(got) != 1731600135 {
		t.Errorf("ended_at = %v, want 1731600135", parsed["ended_at"])
	}
}

func TestWrite_OmitsRunMetadataWhenUnset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	t.Setenv("RESULT_PATH", path)

	if err := Write(Outcome{Status: StatusSuccess}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}

	raw, _ := os.ReadFile(path)
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)

	for _, key := range []string{"model", "started_at", "ended_at", "pr_title"} {
		if _, present := parsed[key]; present {
			t.Errorf("%s should be omitted when unset, got: %v", key, parsed[key])
		}
	}
}

// PRTitle is the agent-produced short title (Bug 2 fix). Distinct from
// ChangesSummary so the runner can pick a clean PR title instead of
// taking the first line of a possibly-long single-line narrative.
func TestWrite_PRTitleRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	t.Setenv("RESULT_PATH", path)

	if err := Write(Outcome{
		Status:         StatusSuccess,
		ChangesSummary: "Reduced the Node heap cap so the dashboard build fits in a 2 GB CI worker.",
		PRTitle:        "Tighten Node heap cap in dashboard build script",
	}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}

	raw, _ := os.ReadFile(path)
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)

	if got, _ := parsed["pr_title"].(string); got != "Tighten Node heap cap in dashboard build script" {
		t.Errorf("pr_title = %v, want %q", parsed["pr_title"], "Tighten Node heap cap in dashboard build script")
	}
	if got, _ := parsed["changes_summary"].(string); !strings.Contains(got, "Reduced the Node heap cap") {
		t.Errorf("changes_summary should round-trip independently of pr_title; got %v", parsed["changes_summary"])
	}
}

func TestWrite_FilesChangedDefaultsToEmptyArray(t *testing.T) {
	// An empty array in JSON is more friendly to consumers than null.
	path := filepath.Join(t.TempDir(), "result.json")
	t.Setenv("RESULT_PATH", path)

	if err := Write(Outcome{Status: StatusSuccess}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}

	raw, _ := os.ReadFile(path)
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)

	filesChanged, ok := parsed["files_changed"].([]any)
	if !ok {
		t.Fatalf("files_changed should be a JSON array, got %T", parsed["files_changed"])
	}
	if len(filesChanged) != 0 {
		t.Errorf("files_changed should be empty, got %v", filesChanged)
	}
}

func TestWrite_OmitsErrorWhenSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	t.Setenv("RESULT_PATH", path)

	if err := Write(Outcome{Status: StatusSuccess, ExitCode: ExitSuccess}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}

	raw, _ := os.ReadFile(path)
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)

	if _, present := parsed["error"]; present {
		t.Errorf("error field should be omitted on success, got: %v", parsed["error"])
	}
}

func TestWriteFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	t.Setenv("RESULT_PATH", path)

	err := WriteFailure(errors.New("something broke"), "summary of attempt")
	if err != nil {
		t.Fatalf("WriteFailure returned error: %v", err)
	}

	raw, _ := os.ReadFile(path)
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)

	if parsed["status"] != string(StatusFailure) {
		t.Errorf("status = %v, want failure", parsed["status"])
	}
	if parsed["error"] != "something broke" {
		t.Errorf("error = %v", parsed["error"])
	}
	if parsed["changes_summary"] != "summary of attempt" {
		t.Errorf("changes_summary = %v", parsed["changes_summary"])
	}
}

// TestWrite_ExitCodeRoundTrips pins the v1.1.4 fix: ExitCode is now
// emitted in result.json (was `json:"-"` pre-v1.1.4). The
// deployment-runner ignores the container's wait exit code and reads
// status/exit_code from result.json, so without this field the runner
// couldn't tell apart "agent crashed" (1) from "auth/rate-limit" (2)
// from "timeout" (4) — every failure looked the same on the dashboard.
func TestWrite_ExitCodeRoundTrips(t *testing.T) {
	cases := []struct {
		name string
		code int
	}{
		{"success", ExitSuccess},
		{"execution_failure", ExitExecutionFailure},
		{"auth_failure", ExitAuthFailure},
		{"cancelled", ExitCancelled},
		{"timeout", ExitTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "result.json")
			t.Setenv("RESULT_PATH", path)
			if err := Write(Outcome{Status: StatusSuccess, ExitCode: tc.code}); err != nil {
				t.Fatalf("Write returned error: %v", err)
			}
			raw, _ := os.ReadFile(path)
			var parsed map[string]any
			if err := json.Unmarshal(raw, &parsed); err != nil {
				t.Fatalf("result.json is not valid JSON: %v", err)
			}
			got, present := parsed["exit_code"].(float64)
			if !present {
				t.Fatalf("exit_code field missing from result.json: %s", string(raw))
			}
			if int(got) != tc.code {
				t.Errorf("exit_code = %d, want %d", int(got), tc.code)
			}
		})
	}
}

func TestPath_Default(t *testing.T) {
	t.Setenv("RESULT_PATH", "")
	if Path() != "/tmp/result.json" {
		t.Errorf("default Path() = %q, want /tmp/result.json", Path())
	}
}

func TestPath_Override(t *testing.T) {
	t.Setenv("RESULT_PATH", "/custom/path.json")
	if Path() != "/custom/path.json" {
		t.Errorf("Path() = %q, want override value", Path())
	}
}
