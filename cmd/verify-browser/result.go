package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// redacted replaces the secret (sign_in's password) in every result file.
const redacted = "<redacted>"

// finish turns the script's outcome into /io/result.json and the exit code.
// With a secret, the script's result is rewritten with it redacted rather
// than moved into place as it is. A completed result's screenshot is moved
// to screenshot.jpg beside it first (keepScreenshot); when it can't be, the
// result is rewritten with "screenshot_error" in place of "screenshot".
func finish(resultPath, tmpResult, tmpScreenshot string, out scriptOutcome, secret string) int {
	tail := out.stderrTail
	if secret != "" && out.stderrCut {
		tail = dropFirstLine(tail)
	}
	switch {
	case out.timedOut:
		return fail(resultPath, errors.New("the browser did not finish within timeout_ms"), tail, secret)
	case out.cancelled:
		return fail(resultPath, errors.New("verify-browser was stopped before the browser finished"), tail, secret)
	}
	res, err := checkScriptResult(tmpResult)
	if err != nil {
		if out.err != nil {
			err = fmt.Errorf("%w (browser script: %v)", err, out.err)
		}
		return fail(resultPath, err, tail, secret)
	}
	screenshotPath := filepath.Join(filepath.Dir(resultPath), "screenshot.jpg")
	screenshotErr := ""
	if res.status == "completed" && res.hasScreenshot {
		screenshotErr = keepScreenshot(tmpScreenshot, screenshotPath)
	}
	switch {
	case screenshotErr != "":
		err = rewriteResult(resultPath, tmpResult, secret, func(r map[string]any) {
			delete(r, "screenshot")
			r["screenshot_error"] = screenshotErr
		})
	case secret == "":
		err = os.Rename(tmpResult, resultPath)
	default:
		err = writeRedacted(resultPath, tmpResult, secret)
	}
	if err != nil {
		// An error result never leaves a screenshot.
		os.Remove(screenshotPath)
		return fail(resultPath, fmt.Errorf("moving the result into place: %w", err), tail, secret)
	}
	if res.status != "completed" {
		return 1
	}
	return 0
}

// maxScreenshotBytes bounds /io/screenshot.jpg.
const maxScreenshotBytes = 1 << 20

// Screenshot errors finish puts in the result in place of its "screenshot".
const (
	screenshotTooLarge    = "too large"
	screenshotNotCaptured = "the page could not be captured"
)

// keepScreenshot renames the script's screenshot to path when it is a JPEG
// of at most maxScreenshotBytes, and returns "". Otherwise it removes it and
// returns the result's screenshot_error.
func keepScreenshot(tmp, path string) string {
	reason := checkScreenshot(tmp)
	if reason == "" && os.Rename(tmp, path) != nil {
		reason = screenshotNotCaptured
	}
	if reason != "" {
		os.Remove(tmp)
	}
	return reason
}

// jpegMarker is how every JPEG file starts: SOI and the next marker's FF.
var jpegMarker = []byte{0xFF, 0xD8, 0xFF}

func checkScreenshot(path string) string {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return screenshotNotCaptured
	}
	if info.Size() > maxScreenshotBytes {
		return screenshotTooLarge
	}
	f, err := os.Open(path)
	if err != nil {
		return screenshotNotCaptured
	}
	defer f.Close()
	head := make([]byte, len(jpegMarker))
	if _, err := io.ReadFull(f, head); err != nil || !bytes.Equal(head, jpegMarker) {
		return screenshotNotCaptured
	}
	return ""
}

// scriptResult is what finish needs to know of the script's result.
type scriptResult struct {
	status string
	// hasScreenshot is true when the result has "screenshot": the script
	// says it wrote one.
	hasScreenshot bool
}

// checkScriptResult reads the script's result and returns its status when it
// is a result.json this program may hand on.
func checkScriptResult(path string) (scriptResult, error) {
	info, err := os.Stat(path)
	if err != nil {
		return scriptResult{}, errors.New("the browser script exited without writing a result")
	}
	if info.Size() > maxResultBytes {
		return scriptResult{}, fmt.Errorf("the browser script's result is larger than %d bytes", maxResultBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return scriptResult{}, fmt.Errorf("reading the browser script's result: %w", err)
	}
	var r struct {
		SchemaVersion int             `json:"schema_version"`
		Status        string          `json:"status"`
		Verdict       string          `json:"verdict"`
		Screenshot    json.RawMessage `json:"screenshot"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return scriptResult{}, fmt.Errorf("the browser script's result is not valid JSON: %w", err)
	}
	if r.SchemaVersion != schemaVersion {
		return scriptResult{}, fmt.Errorf("the browser script's result has schema_version %d, want %d", r.SchemaVersion, schemaVersion)
	}
	switch {
	case r.Status == "error":
	case r.Status == "completed" && (r.Verdict == "pass" || r.Verdict == "fail"):
	default:
		return scriptResult{}, fmt.Errorf("the browser script's result has status %q and verdict %q", r.Status, r.Verdict)
	}
	return scriptResult{status: r.Status, hasScreenshot: r.Screenshot != nil}, nil
}

// dropFirstLine drops a cut stderr tail's text up to and including its first
// line break: the start of a line the cut went through, which could hold
// the end of the secret, too short to be found and redacted.
func dropFirstLine(tail string) string {
	if i := strings.IndexByte(tail, '\n'); i >= 0 {
		return tail[i+1:]
	}
	return ""
}

// writeRedacted writes the script's result to path with every occurrence of
// secret replaced, in every string value and object key at any depth. It
// works on the decoded strings, not the file's bytes, so the secret is found
// however the script escaped it; numbers are kept as written.
func writeRedacted(path, scriptResult, secret string) error {
	return rewriteResult(path, scriptResult, secret, nil)
}

// rewriteResult writes the script's result to path re-encoded, after edit
// (when not nil) changed it, with secret redacted when it is not "" (as
// writeRedacted does). Numbers are kept as written.
func rewriteResult(path, scriptResult, secret string, edit func(map[string]any)) error {
	data, err := os.ReadFile(scriptResult)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("the browser script's result is not valid JSON: %w", err)
	}
	if edit != nil {
		r, ok := v.(map[string]any)
		if !ok {
			return errors.New("the browser script's result is not a JSON object")
		}
		edit(r)
	}
	if secret != "" {
		// Never with "": ReplaceAll would put the mark between every character.
		v = redactValue(v, secret)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	return writeAtomic(path, buf.Bytes())
}

// redactValue replaces secret in v's strings and object keys.
func redactValue(v any, secret string) any {
	switch x := v.(type) {
	case string:
		return strings.ReplaceAll(x, secret, redacted)
	case []any:
		for i := range x {
			x[i] = redactValue(x[i], secret)
		}
		return x
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[strings.ReplaceAll(k, secret, redacted)] = redactValue(val, secret)
		}
		return out
	}
	return v
}

// writeErrorResult writes a status "error" result.json atomically, with
// secret (when not "") redacted from its message and the stderr tail.
func writeErrorResult(path, msg, stderrTail, secret string) error {
	// Redact each part before trimming or joining, so neither can change a
	// password at their edges (one ending in a space, say) out of recognition.
	if secret != "" {
		msg = strings.ReplaceAll(msg, secret, redacted)
		stderrTail = strings.ReplaceAll(stderrTail, secret, redacted)
	}
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
