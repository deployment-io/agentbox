package result

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The review half of result.json is a contract three components read. These
// pin the field names and, more importantly, the field that must NOT exist.

func TestWriteEmitsTheReviewResultShape(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RESULT_PATH", filepath.Join(dir, "result.json"))

	err := Write(Outcome{
		Status:   StatusSuccess,
		ExitCode: ExitSuccess,
		ReviewResult: &ReviewResult{
			Findings: []ReviewFinding{{
				Key: "sec-1", Parameter: "security", Severity: "high",
				Location: "0-acme/api/handler.go:41", What: "no auth check",
				Why: "any caller can read another org's data", Stage: "review", Pass: "security",
			}},
			Coverage: []ReviewCoverage{{Parameter: "testing", State: "not checked", Reason: "no pass"}},
		},
	})
	if err != nil {
		t.Fatalf("Write: %s", err)
	}

	raw, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("result.json is not valid JSON: %s", err)
	}
	review, ok := doc["review_result"].(map[string]interface{})
	if !ok {
		t.Fatalf("result.json has no review_result object: %s", raw)
	}
	finding := review["findings"].([]interface{})[0].(map[string]interface{})
	for _, key := range []string{"key", "parameter", "severity", "location", "what", "why", "stage", "pass"} {
		if _, ok := finding[key]; !ok {
			t.Errorf("finding is missing %q — the consumer reads by name: %v", key, finding)
		}
	}
	coverage := review["coverage"].([]interface{})[0].(map[string]interface{})
	for _, key := range []string{"parameter", "state", "reason"} {
		if _, ok := coverage[key]; !ok {
			t.Errorf("coverage entry is missing %q: %v", key, coverage)
		}
	}
	// A round given no open findings reports no status list at all.
	if _, ok := review["previous"]; ok {
		t.Errorf("review_result carries a previous list with nothing given: %v", review)
	}
}

// The status of each previously reported finding travels in result.json under
// "previous". The runner matches it by key against what it handed over, and
// reads a key it does not get back as still present.
func TestWriteEmitsThePreviousStatuses(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RESULT_PATH", filepath.Join(dir, "result.json"))

	err := Write(Outcome{
		Status:   StatusSuccess,
		ExitCode: ExitSuccess,
		ReviewResult: &ReviewResult{
			Findings: []ReviewFinding{},
			Coverage: []ReviewCoverage{{Parameter: "security", State: "checked"}},
			Previous: []PreviousFinding{
				{Key: "sec-unauthenticated-env-dump-app-js", Status: "still_present", Note: "the route is unchanged"},
				{Key: "cor-missing-nil-check", Status: "resolved"},
			},
		},
	})
	if err != nil {
		t.Fatalf("Write: %s", err)
	}

	raw, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("result.json is not valid JSON: %s", err)
	}
	previous, ok := doc["review_result"].(map[string]interface{})["previous"].([]interface{})
	if !ok || len(previous) != 2 {
		t.Fatalf("review_result has no two-entry previous list: %s", raw)
	}
	first := previous[0].(map[string]interface{})
	for _, key := range []string{"key", "status", "note"} {
		if _, ok := first[key]; !ok {
			t.Errorf("previous entry is missing %q — the consumer reads by name: %v", key, first)
		}
	}
	if second := previous[1].(map[string]interface{}); second["status"] != "resolved" {
		t.Errorf("previous[1] = %v, want the resolved status", second)
	}
}

// must_fix_open is the decision about whether the work may proceed, and it is
// the RUNNER's to make from the org's thresholds. If this struct ever grows
// the field, an agent could assert it and wave its own findings through.
func TestReviewResultCannotCarryMustFixOpen(t *testing.T) {
	encoded, err := json.Marshal(ReviewResult{Findings: []ReviewFinding{{What: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(encoded)), "must_fix") {
		t.Errorf("review_result carries a must-fix field: %s — that decision is not agentbox's to emit", encoded)
	}
}

// A run that is not a review must not mention the object at all, so nothing
// downstream mistakes an implement run for a reviewed one.
func TestWriteOmitsReviewResultForAnImplementRun(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RESULT_PATH", filepath.Join(dir, "result.json"))

	if err := Write(Outcome{Status: StatusSuccess}); err != nil {
		t.Fatalf("Write: %s", err)
	}
	raw, _ := os.ReadFile(Path())
	if strings.Contains(string(raw), "review_result") {
		t.Errorf("result.json mentions review_result for a non-review run: %s", raw)
	}
}

// A review run that fails before the agent starts still writes a
// review_result, so the consumer records why rather than finding nothing.
func TestWriteReviewFailureCarriesCoverage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RESULT_PATH", filepath.Join(dir, "result.json"))
	coverage := []ReviewCoverage{{Parameter: "security", State: "not checked", Reason: "config load failed: boom"}}
	if err := WriteReviewFailure(errors.New("boom"), coverage); err != nil {
		t.Fatalf("WriteReviewFailure: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out["status"] != "failure" {
		t.Errorf("status = %v, want failure", out["status"])
	}
	rr, ok := out["review_result"].(map[string]any)
	if !ok {
		t.Fatalf("review_result missing from a review-mode failure: %s", b)
	}
	if f, _ := rr["findings"].([]any); f == nil {
		t.Errorf("findings must be an empty array, not absent/null: %s", b)
	}
	if c, _ := rr["coverage"].([]any); len(c) != 1 {
		t.Errorf("coverage = %v, want the one entry supplied", rr["coverage"])
	}
}
