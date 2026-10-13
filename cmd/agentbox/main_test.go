package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/deployment-io/agentbox/internal/config"
	"github.com/deployment-io/agentbox/internal/result"
)

func readResult(t *testing.T, path string) result.Outcome {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out result.Outcome
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWriteFailureResultInQAModeIsCouldntRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	t.Setenv("RESULT_PATH", path)
	t.Setenv("AGENT_MODE", config.ModeQA)

	writeFailureResult("config error", errors.New("QA_REPOSITORIES is required in qa mode"))

	out := readResult(t, path)
	if out.Status != result.StatusFailure || out.ExitCode != result.ExitExecutionFailure {
		t.Errorf("status = %q, exit = %d", out.Status, out.ExitCode)
	}
	qr := out.QAResult
	if qr == nil {
		t.Fatal("no qa_result in a qa-mode failure")
	}
	if qr.Status != result.QAStatusCouldntRun || qr.StatusReason != "config error: QA_REPOSITORIES is required in qa mode" {
		t.Errorf("qa_result = %+v", qr)
	}
	if len(qr.Findings) != 0 {
		t.Errorf("a failure carries findings: %+v", qr.Findings)
	}
	if out.ReviewResult != nil {
		t.Errorf("a qa failure carries a review_result")
	}
}

func TestWriteFailureResultInBatchModeHasNoQAResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	t.Setenv("RESULT_PATH", path)
	t.Setenv("AGENT_MODE", "")

	writeFailureResult("config error", errors.New("boom"))

	if out := readResult(t, path); out.QAResult != nil || out.ReviewResult != nil {
		t.Errorf("batch failure = %+v", out)
	}
}
