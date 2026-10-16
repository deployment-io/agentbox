package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// maxResultBytes bounds what is read back from the script.
const maxResultBytes = 4 << 20

// errorResultJSON is result.json for a check that couldn't run.
// BlockedRequests is always present; this program never saw the browser's
// requests, so it is always empty here.
type errorResultJSON struct {
	SchemaVersion   int              `json:"schema_version"`
	Status          string           `json:"status"`
	Error           string           `json:"error"`
	BlockedRequests []blockedRequest `json:"blocked_requests"`
}

// blockedRequest is a blocked_requests entry.
type blockedRequest struct {
	URL    string `json:"url"`
	Method string `json:"method"`
}

// finish turns the script's outcome into /io/result.json and the exit code.
func finish(resultPath, tmpResult string, out scriptOutcome) int {
	switch {
	case out.timedOut:
		return fail(resultPath, errors.New("the browser did not finish within timeout_ms"), out.stderrTail)
	case out.cancelled:
		return fail(resultPath, errors.New("verify-browser was stopped before the browser finished"), out.stderrTail)
	}
	status, err := checkScriptResult(tmpResult)
	if err != nil {
		if out.err != nil {
			err = fmt.Errorf("%w (browser script: %v)", err, out.err)
		}
		return fail(resultPath, err, out.stderrTail)
	}
	if err := os.Rename(tmpResult, resultPath); err != nil {
		return fail(resultPath, fmt.Errorf("moving the result into place: %w", err), out.stderrTail)
	}
	if status != "completed" {
		return 1
	}
	return 0
}

// checkScriptResult reads the script's result and returns its status when it
// is a result.json this program may hand on.
func checkScriptResult(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", errors.New("the browser script exited without writing a result")
	}
	if info.Size() > maxResultBytes {
		return "", fmt.Errorf("the browser script's result is larger than %d bytes", maxResultBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading the browser script's result: %w", err)
	}
	var r struct {
		SchemaVersion int    `json:"schema_version"`
		Status        string `json:"status"`
		Verdict       string `json:"verdict"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return "", fmt.Errorf("the browser script's result is not valid JSON: %w", err)
	}
	if r.SchemaVersion != schemaVersion {
		return "", fmt.Errorf("the browser script's result has schema_version %d, want %d", r.SchemaVersion, schemaVersion)
	}
	switch {
	case r.Status == "error":
	case r.Status == "completed" && (r.Verdict == "pass" || r.Verdict == "fail"):
	default:
		return "", fmt.Errorf("the browser script's result has status %q and verdict %q", r.Status, r.Verdict)
	}
	return r.Status, nil
}

// writeErrorResult writes a status "error" result.json atomically.
func writeErrorResult(path, msg, stderrTail string) error {
	if tail := strings.TrimSpace(stderrTail); tail != "" {
		msg += "\nlast output of the browser script:\n" + tail
	}
	data, err := json.Marshal(errorResultJSON{
		SchemaVersion:   schemaVersion,
		Status:          "error",
		Error:           msg,
		BlockedRequests: []blockedRequest{},
	})
	if err != nil {
		return err
	}
	return writeAtomic(path, data)
}

// writeAtomic writes data to a temp file beside path and renames it over
// path, so a reader never sees a partial file.
func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".result-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
