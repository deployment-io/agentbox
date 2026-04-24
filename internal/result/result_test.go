package result

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
