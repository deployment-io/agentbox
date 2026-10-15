package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deployment-io/agentbox/internal/proxy"
)

const validInputJSON = `{
  "schema_version": 1,
  "url": "https://preview-abc.example.net/",
  "allowed_hosts": ["preview-abc.example.net"],
  "steps": [
    {"action": "goto", "url": "https://preview-abc.example.net/about"},
    {"action": "click", "selector": "text=More"},
    {"action": "fill", "selector": "#q", "value": ""},
    {"action": "press", "selector": "#q", "key": "Enter"},
    {"action": "wait_for", "text": "Results"},
    {"action": "wait_for", "selector": "#results"}
  ],
  "assertions": [
    {"type": "text_visible", "text": "Results"},
    {"type": "text_absent", "text": "Error"},
    {"type": "selector_visible", "selector": "#results"},
    {"type": "url_path", "path": "/about"}
  ],
  "timeout_ms": 30000
}`

// mutate decodes validInputJSON into a generic map, lets fn change it and
// re-encodes it.
func mutate(t *testing.T, fn func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(validInputJSON), &m); err != nil {
		t.Fatal(err)
	}
	fn(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseInputAcceptsValidInput(t *testing.T) {
	in, err := parseInput([]byte(validInputJSON))
	if err != nil {
		t.Fatalf("parseInput: %v", err)
	}
	if len(in.Steps) != 6 || len(in.Assertions) != 4 {
		t.Fatalf("got %d steps, %d assertions", len(in.Steps), len(in.Assertions))
	}
}

func TestParseInputRejects(t *testing.T) {
	long := strings.Repeat("a", maxStringChars+1)
	many := func(n int, v map[string]any) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = v
		}
		return out
	}
	cases := map[string]func(m map[string]any){
		"schema version":            func(m map[string]any) { m["schema_version"] = 2 },
		"non-https url":             func(m map[string]any) { m["url"] = "http://preview-abc.example.net/" },
		"url outside allowed_hosts": func(m map[string]any) { m["url"] = "https://other.example.net/" },
		"url with port":             func(m map[string]any) { m["url"] = "https://preview-abc.example.net:8443/" },
		"url with user info":        func(m map[string]any) { m["url"] = "https://u:p@preview-abc.example.net/" },
		"no allowed hosts":          func(m map[string]any) { m["allowed_hosts"] = []any{} },
		"allowed host with port":    func(m map[string]any) { m["allowed_hosts"] = []any{"preview-abc.example.net:443"} },
		"goto outside allowed_hosts": func(m map[string]any) {
			m["steps"] = []any{map[string]any{"action": "goto", "url": "https://www.iana.org/"}}
		},
		"goto non-https": func(m map[string]any) {
			m["steps"] = []any{map[string]any{"action": "goto", "url": "http://preview-abc.example.net/"}}
		},
		"unknown action": func(m map[string]any) {
			m["steps"] = []any{map[string]any{"action": "hover", "selector": "a"}}
		},
		"click without selector": func(m map[string]any) { m["steps"] = []any{map[string]any{"action": "click"}} },
		"fill without value": func(m map[string]any) {
			m["steps"] = []any{map[string]any{"action": "fill", "selector": "#q"}}
		},
		"press without key": func(m map[string]any) {
			m["steps"] = []any{map[string]any{"action": "press", "selector": "#q"}}
		},
		"wait_for with both": func(m map[string]any) {
			m["steps"] = []any{map[string]any{"action": "wait_for", "selector": "a", "text": "b"}}
		},
		"wait_for with neither": func(m map[string]any) { m["steps"] = []any{map[string]any{"action": "wait_for"}} },
		"unknown assertion": func(m map[string]any) {
			m["assertions"] = []any{map[string]any{"type": "title_is", "text": "x"}}
		},
		"relative url_path": func(m map[string]any) {
			m["assertions"] = []any{map[string]any{"type": "url_path", "path": "about"}}
		},
		"too many steps": func(m map[string]any) {
			m["steps"] = many(maxSteps+1, map[string]any{"action": "click", "selector": "a"})
		},
		"too many assertions": func(m map[string]any) {
			m["assertions"] = many(maxAssertions+1, map[string]any{"type": "text_visible", "text": "a"})
		},
		"long unused step field": func(m map[string]any) {
			m["steps"] = []any{map[string]any{"action": "click", "selector": "a", "value": long}}
		},
		"long unused assertion field": func(m map[string]any) {
			m["assertions"] = []any{map[string]any{"type": "text_visible", "text": "a", "path": long}}
		},
		"long string": func(m map[string]any) {
			m["assertions"] = []any{map[string]any{"type": "text_visible", "text": long}}
		},
		"timeout too short": func(m map[string]any) { m["timeout_ms"] = minTimeoutMS - 1 },
		"timeout too long":  func(m map[string]any) { m["timeout_ms"] = maxTimeoutMS + 1 },
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseInput(mutate(t, fn)); err == nil {
				t.Fatal("parseInput accepted it")
			}
		})
	}
	if _, err := parseInput([]byte("{not json")); err == nil {
		t.Fatal("parseInput accepted invalid JSON")
	}
}

func TestParseInputAcceptsLimits(t *testing.T) {
	in := mutate(t, func(m map[string]any) {
		steps := make([]any, maxSteps)
		for i := range steps {
			steps[i] = map[string]any{"action": "click", "selector": strings.Repeat("é", maxStringChars)}
		}
		m["steps"] = steps
		m["timeout_ms"] = maxTimeoutMS
		m["url"] = "https://PREVIEW-abc.example.net:443/x"
	})
	if _, err := parseInput(in); err != nil {
		t.Fatalf("parseInput: %v", err)
	}
}

func TestParseInputViewport(t *testing.T) {
	good := []any{
		map[string]any{"preset": "desktop"},
		map[string]any{"preset": "tablet"},
		map[string]any{"preset": "mobile"},
		map[string]any{"width": 320, "height": 480},
		map[string]any{"width": 2560, "height": 1600},
		map[string]any{"width": 1279.0, "height": 720},
	}
	for _, v := range good {
		if _, err := parseInput(mutate(t, func(m map[string]any) { m["viewport"] = v })); err != nil {
			t.Errorf("viewport %v: %v", v, err)
		}
	}
	bad := map[string]any{
		"width 319":      map[string]any{"width": 319, "height": 600},
		"width 2561":     map[string]any{"width": 2561, "height": 600},
		"height 479":     map[string]any{"width": 800, "height": 479},
		"height 1601":    map[string]any{"width": 800, "height": 1601},
		"fractional":     map[string]any{"width": 800.5, "height": 600},
		"string height":  map[string]any{"width": 800, "height": "600"},
		"unknown preset": map[string]any{"preset": "watch"},
		"both forms":     map[string]any{"preset": "mobile", "width": 800, "height": 600},
		"width alone":    map[string]any{"width": 800},
		"unknown key":    map[string]any{"preset": "mobile", "orientation": "landscape"},
		"empty object":   map[string]any{},
		"null":           nil,
		"not an object":  "mobile",
	}
	for name, v := range bad {
		t.Run(name, func(t *testing.T) {
			_, err := parseInput(mutate(t, func(m map[string]any) { m["viewport"] = v }))
			if err == nil || !strings.Contains(err.Error(), "viewport") {
				t.Fatalf("parseInput: %v, want an error naming viewport", err)
			}
		})
	}
}

func TestParseInputReadOnly(t *testing.T) {
	in, err := parseInput([]byte(validInputJSON))
	if err != nil || in.ReadOnly != nil {
		t.Fatalf("without read_only: %v, read_only = %s — absent must mean false", err, in.ReadOnly)
	}
	for _, v := range []bool{true, false} {
		if _, err := parseInput(mutate(t, func(m map[string]any) { m["read_only"] = v })); err != nil {
			t.Errorf("read_only %v: %v", v, err)
		}
	}
	for name, v := range map[string]any{"null": nil, "string": "true", "number": 1, "object": map[string]any{}} {
		t.Run(name, func(t *testing.T) {
			_, err := parseInput(mutate(t, func(m map[string]any) { m["read_only"] = v }))
			if err == nil || !strings.Contains(err.Error(), "read_only") {
				t.Fatalf("parseInput: %v, want an error naming read_only", err)
			}
		})
	}
}

// harness is an /io directory and a fake browser script run by sh.
type harness struct {
	t   *testing.T
	cfg config
}

func newHarness(t *testing.T, script string) *harness {
	t.Helper()
	dir := t.TempDir()
	ioDir := filepath.Join(dir, "io")
	if err := os.Mkdir(ioDir, 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(dir, "fake.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ioDir, "input.json"), []byte(validInputJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, cfg: config{ioDir: ioDir, node: "/bin/sh", script: scriptPath, grace: 200 * time.Millisecond}}
}

func (h *harness) run() (int, map[string]any) {
	h.t.Helper()
	code := run(context.Background(), h.cfg)
	data, err := os.ReadFile(filepath.Join(h.cfg.ioDir, "result.json"))
	if err != nil {
		h.t.Fatalf("no result.json: %v", err)
	}
	var r map[string]any
	if err := json.Unmarshal(data, &r); err != nil {
		h.t.Fatalf("result.json is not JSON: %v\n%s", err, data)
	}
	return code, r
}

// ioEntries lists what is left in /io.
func (h *harness) ioEntries() []string {
	entries, err := os.ReadDir(h.cfg.ioDir)
	if err != nil {
		h.t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func wantError(t *testing.T, code int, r map[string]any, contains ...string) {
	t.Helper()
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if r["status"] != "error" || r["schema_version"] != float64(1) {
		t.Fatalf("result = %v, want status error, schema_version 1", r)
	}
	msg, _ := r["error"].(string)
	for _, c := range contains {
		if !strings.Contains(msg, c) {
			t.Errorf("error %q does not contain %q", msg, c)
		}
	}
}

func TestRunRenamesTheScriptResult(t *testing.T) {
	h := newHarness(t, `printf '{"schema_version":1,"status":"completed","verdict":"pass","proxy":"%s"}' "$3" > "$2"`)
	code, r := h.run()
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if r["status"] != "completed" || r["verdict"] != "pass" {
		t.Fatalf("result = %v", r)
	}
	if proxyAddr, _ := r["proxy"].(string); !strings.HasPrefix(proxyAddr, "127.0.0.1:") {
		t.Errorf("script got proxy address %q", proxyAddr)
	}
	if got := h.ioEntries(); len(got) != 2 {
		t.Errorf("/io holds %v, want only input.json and result.json", got)
	}
}

func TestRunPassesOnAScriptErrorResult(t *testing.T) {
	h := newHarness(t, `printf '{"schema_version":1,"status":"error","error":"launch failed"}' > "$2"`)
	code, r := h.run()
	wantError(t, code, r, "launch failed")
}

func TestRunWithoutAResult(t *testing.T) {
	h := newHarness(t, `echo "chromium crashed" >&2; exit 3`)
	code, r := h.run()
	wantError(t, code, r, "without writing a result", "chromium crashed")
}

func TestRunWithInvalidJSON(t *testing.T) {
	h := newHarness(t, `printf 'not json' > "$2"`)
	code, r := h.run()
	wantError(t, code, r, "not valid JSON")
}

func TestRunWithWrongSchemaVersion(t *testing.T) {
	h := newHarness(t, `printf '{"schema_version":2,"status":"completed","verdict":"pass"}' > "$2"`)
	code, r := h.run()
	wantError(t, code, r, "schema_version 2")
}

func TestRunTimeout(t *testing.T) {
	// Ignores SIGTERM, so it is killed after the grace; a result written
	// after the deadline does not count.
	h := newHarness(t, `trap '' TERM; echo "still loading" >&2; sleep 2; printf '{"schema_version":1,"status":"completed","verdict":"pass"}' > "$2"`)
	h.cfg.timeout = 300 * time.Millisecond
	start := time.Now()
	code, r := h.run()
	wantError(t, code, r, "timeout_ms", "still loading")
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("run took %s; the script was not killed after the grace", elapsed)
	}
}

func TestRunInvalidInputIsAnErrorResult(t *testing.T) {
	h := newHarness(t, `exit 0`)
	if err := os.WriteFile(filepath.Join(h.cfg.ioDir, "input.json"), []byte(`{"schema_version":1,"url":"http://x/"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	code, r := h.run()
	wantError(t, code, r, "invalid input")
}

func TestRunStartsTheProxyBlockingPrivateIPs(t *testing.T) {
	var gotCfg proxy.Config
	var gotAllow *proxy.AllowList
	orig := startProxy
	t.Cleanup(func() { startProxy = orig })
	startProxy = func(allow *proxy.AllowList, cfg proxy.Config) (*proxy.Server, error) {
		gotAllow, gotCfg = allow, cfg
		return orig(allow, cfg)
	}
	h := newHarness(t, `printf '{"schema_version":1,"status":"completed","verdict":"fail"}' > "$2"`)
	if code, _ := h.run(); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if !gotCfg.BlockPrivateIPs {
		t.Error("proxy started without BlockPrivateIPs")
	}
	if gotAllow == nil || !gotAllow.Allows("cdn.example.com") || gotAllow.Allows("localhost") {
		t.Error("proxy not started with the allow-all list")
	}
}

func TestWriteAtomicLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "result.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeErrorResult(path, "boom", "tail"); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("dir holds %d entries, want 1", len(entries))
	}
	data, _ := os.ReadFile(path)
	var r errorResultJSON
	if err := json.Unmarshal(data, &r); err != nil || r.Status != "error" || !strings.Contains(r.Error, "boom") {
		t.Fatalf("result = %s (%v)", data, err)
	}
	if !strings.Contains(string(data), `"blocked_requests":[]`) {
		t.Errorf("result = %s, want blocked_requests []", data)
	}
}
