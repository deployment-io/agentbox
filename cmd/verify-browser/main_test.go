package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
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

func TestParseInputBackStep(t *testing.T) {
	in, err := parseInput(mutate(t, func(m map[string]any) {
		m["steps"] = []any{map[string]any{"action": "back"}}
	}))
	if err != nil {
		t.Fatalf("parseInput: %v", err)
	}
	if len(in.Steps) != 1 || in.Steps[0].Action != "back" {
		t.Fatalf("got steps %+v", in.Steps)
	}
	_, err = parseInput(mutate(t, func(m map[string]any) {
		m["steps"] = []any{map[string]any{"action": "hover", "selector": "a"}}
	}))
	if err == nil || !strings.Contains(err.Error(), "wait_for or back") {
		t.Fatalf("unknown action error = %v, want it to list back", err)
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
	if err := writeErrorResult(path, "boom", "tail", ""); err != nil {
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

// testPassword needs escaping in JSON: a quote, a backslash, "<" and a
// non-ASCII character.
const testPassword = `pa"ss\<wörd>`

var testSignIn = map[string]any{
	"url":      "https://preview-abc.example.net/login",
	"email":    "qa-admin@test.example",
	"password": testPassword,
}

func withSignIn(m map[string]any) { m["sign_in"] = maps.Clone(testSignIn) }

func TestParseInputSignIn(t *testing.T) {
	in, err := parseInput(mutate(t, withSignIn))
	if err != nil {
		t.Fatalf("parseInput: %v", err)
	}
	if in.signIn == nil || in.signIn.Password != testPassword || in.signIn.Email != "qa-admin@test.example" || in.secret() != testPassword {
		t.Fatalf("signIn = %+v", in.signIn)
	}
	if in, err := parseInput([]byte(validInputJSON)); err != nil || in.signIn != nil || in.secret() != "" {
		t.Fatalf("without sign_in: %v, %+v", err, in.signIn)
	}
	longest := mutate(t, func(m map[string]any) {
		withSignIn(m)
		m["sign_in"].(map[string]any)["email"] = strings.Repeat("e", maxEmailChars)
		m["sign_in"].(map[string]any)["password"] = strings.Repeat("é", maxPasswordChars)
	})
	if _, err := parseInput(longest); err != nil {
		t.Fatalf("longest credentials: %v", err)
	}
}

func TestParseInputRejectsSignIn(t *testing.T) {
	set := func(key string, v any) func(m map[string]any) {
		return func(m map[string]any) {
			withSignIn(m)
			m["sign_in"].(map[string]any)[key] = v
		}
	}
	del := func(key string) func(m map[string]any) {
		return func(m map[string]any) {
			withSignIn(m)
			delete(m["sign_in"].(map[string]any), key)
		}
	}
	cases := map[string]func(m map[string]any){
		"url outside allowed_hosts":  set("url", "https://other.example.net/login"),
		"http url":                   set("url", "http://preview-abc.example.net/login"),
		"url with port":              set("url", "https://preview-abc.example.net:8443/login"),
		"missing url":                del("url"),
		"missing email":              del("email"),
		"missing password":           del("password"),
		"empty email":                set("email", ""),
		"empty password":             set("password", ""),
		"null password":              set("password", nil),
		"number password":            set("password", 1234),
		"line break in the email":    set("email", "qa@test.example\n"),
		"line break in the password": set("password", "pass\rword"),
		"unicode line break":         set("password", "pass\u2028word"),
		"long email":                 set("email", strings.Repeat("e", maxEmailChars+1)),
		"long password":              set("password", strings.Repeat("p", maxPasswordChars+1)),
		"unknown key":                set("otp", "123456"),
		"null":                       func(m map[string]any) { m["sign_in"] = nil },
		"not an object":              func(m map[string]any) { m["sign_in"] = "x" },
		"with read_only true": func(m map[string]any) {
			withSignIn(m)
			m["read_only"] = true
		},
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseInput(mutate(t, fn))
			if err == nil || !strings.Contains(err.Error(), "sign_in") {
				t.Fatalf("parseInput: %v, want an error naming sign_in", err)
			}
			if strings.Contains(err.Error(), testPassword) {
				t.Fatalf("error %q quotes the password", err)
			}
		})
	}
	if _, err := parseInput(mutate(t, func(m map[string]any) { withSignIn(m); m["read_only"] = false })); err != nil {
		t.Fatalf("sign_in with read_only false: %v", err)
	}
}

// newSignInHarness is a harness whose input has sign_in.
func newSignInHarness(t *testing.T, script string) *harness {
	t.Helper()
	h := newHarness(t, script)
	if err := os.WriteFile(filepath.Join(h.cfg.ioDir, "input.json"), mutate(t, withSignIn), 0o644); err != nil {
		t.Fatal(err)
	}
	return h
}

// scriptWriting is a fake script that writes data as its result.
func scriptWriting(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "result.json")
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`cat %q > "$2"`, path)
}

func TestRunRedactsThePasswordFromTheResult(t *testing.T) {
	// The password as the script might write it: escaped two ways, in a
	// nested string value and in an object key.
	result := `{"schema_version":1,"status":"completed","verdict":"pass",` +
		`"n":1.50,"big":12345678901234567890,"email":"qa-admin@test.example",` +
		`"nested":{"list":["typed pa\"ss\\\u003cw\u00f6rd> here"],` +
		`"key pa\"ss\\<wörd>":"v"}}`
	var check map[string]any
	if err := json.Unmarshal([]byte(result), &check); err != nil {
		t.Fatalf("test result is not JSON: %v", err)
	}
	h := newSignInHarness(t, scriptWriting(t, result))
	code, r := h.run()
	if code != 0 {
		t.Fatalf("exit code = %d, result %v", code, r)
	}
	data, err := os.ReadFile(filepath.Join(h.cfg.ioDir, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	nested, _ := r["nested"].(map[string]any)
	list, _ := nested["list"].([]any)
	if len(list) != 1 || list[0] != "typed <redacted> here" {
		t.Errorf("nested value = %v, want the password redacted", nested["list"])
	}
	if nested["key <redacted>"] != "v" {
		t.Errorf("nested = %v, want the key redacted", nested)
	}
	if strings.Contains(string(data), "ss\\") || strings.Contains(string(data), "rd>") {
		t.Errorf("result still holds the password: %s", data)
	}
	if !bytes.Contains(data, []byte("<redacted>")) {
		t.Errorf("result = %s, want <redacted> unescaped", data)
	}
	if !bytes.Contains(data, []byte(`"n":1.50`)) || !bytes.Contains(data, []byte(`"big":12345678901234567890`)) {
		t.Errorf("result = %s, want numbers as written", data)
	}
	if r["email"] != "qa-admin@test.example" {
		t.Errorf("email = %v, want it left as it is", r["email"])
	}
	if got := h.ioEntries(); len(got) != 2 {
		t.Errorf("/io holds %v, want only input.json and result.json", got)
	}
}

func TestRunWithoutSignInWritesTheResultAsItIs(t *testing.T) {
	result := `{"schema_version":1, "status":"completed","verdict":"pass","x":"<&> pa\"ss\\<wörd>","n":1.50}`
	h := newHarness(t, scriptWriting(t, result))
	if code, _ := h.run(); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	data, err := os.ReadFile(filepath.Join(h.cfg.ioDir, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != result {
		t.Fatalf("result = %s, want byte-for-byte %s", data, result)
	}
}

func TestRunRedactsThePasswordFromAnErrorResult(t *testing.T) {
	h := newSignInHarness(t, `printf 'typed pa"ss\\<wörd> into the field\n' >&2; exit 3`)
	code, r := h.run()
	wantError(t, code, r, "without writing a result", "typed <redacted> into the field")
	if msg, _ := r["error"].(string); strings.Contains(msg, testPassword) {
		t.Errorf("error %q holds the password", msg)
	}
}

func TestRunInvalidSignInNeverShowsThePassword(t *testing.T) {
	h := newHarness(t, `exit 0`)
	in := mutate(t, func(m map[string]any) {
		withSignIn(m)
		m["sign_in"].(map[string]any)["url"] = "https://qa:secret-pw@preview-abc.example.net/login"
		m["sign_in"].(map[string]any)["password"] = "secret-pw"
	})
	if err := os.WriteFile(filepath.Join(h.cfg.ioDir, "input.json"), in, 0o644); err != nil {
		t.Fatal(err)
	}
	code, r := h.run()
	wantError(t, code, r, "invalid input", "sign_in.url")
	if msg, _ := r["error"].(string); strings.Contains(msg, "secret-pw") {
		t.Errorf("error %q holds the password", msg)
	}
	// An invalid input is redacted with whatever password it carries.
	if got := rawSecret(in); got != "secret-pw" {
		t.Errorf("rawSecret = %q", got)
	}
	if got := rawSecret([]byte(`{"sign_in":{"password":1}}`)); got != "" {
		t.Errorf("rawSecret of a number = %q", got)
	}
}

func TestRunRedactsAScriptErrorResult(t *testing.T) {
	h := newSignInHarness(t, scriptWriting(t, `{"schema_version":1,"status":"error","error":"bad pa\"ss\\<wörd>","blocked_requests":[]}`))
	code, r := h.run()
	wantError(t, code, r, "bad <redacted>")
}

func TestRunDropsACutStderrTailsFirstLine(t *testing.T) {
	script := `head -c 1500 /dev/zero | tr '\0' 'a' >&2; printf 'ss\\<wörd> ends the cut line\nlast line\n' >&2; exit 3`
	h := newSignInHarness(t, script)
	code, r := h.run()
	wantError(t, code, r, "last line")
	if msg, _ := r["error"].(string); strings.Contains(msg, "aaa") || strings.Contains(msg, "cut line") {
		t.Errorf("error %q keeps the cut tail's first line", msg)
	}
	// Without sign_in, the tail is kept as today.
	h = newHarness(t, script)
	code, r = h.run()
	wantError(t, code, r, "aaa", "cut line", "last line")
}

func TestWriteErrorResultRedacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	if err := writeErrorResult(path, "fill "+testPassword+" failed", "tail "+testPassword, testPassword); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var r errorResultJSON
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.Error, testPassword) || strings.Count(r.Error, redacted) != 2 {
		t.Fatalf("error = %q, want the password redacted twice", r.Error)
	}
}

func TestWriteErrorResultRedactsBeforeTrimming(t *testing.T) {
	const pw = "secret-pw "
	path := filepath.Join(t.TempDir(), "result.json")
	if err := writeErrorResult(path, "failed", "typing "+pw, pw); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var r errorResultJSON
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.Error, "secret-pw") || !strings.HasSuffix(r.Error, "typing "+redacted) {
		t.Fatalf("error = %q, want the password ending the tail redacted", r.Error)
	}
}

func TestTailBufferReportsACut(t *testing.T) {
	tb := &tailBuffer{max: 4}
	tb.Write([]byte("abcd"))
	if s, cut := tb.contents(); s != "abcd" || cut {
		t.Fatalf("contents = %q, %v", s, cut)
	}
	tb.Write([]byte("e"))
	if s, cut := tb.contents(); s != "bcde" || !cut {
		t.Fatalf("contents = %q, %v", s, cut)
	}
	if got := dropFirstLine("ab\ncd\n"); got != "cd\n" {
		t.Errorf("dropFirstLine = %q", got)
	}
	if got := dropFirstLine("no break"); got != "" {
		t.Errorf("dropFirstLine = %q", got)
	}
}

func TestParseInputScreenshot(t *testing.T) {
	in, err := parseInput([]byte(validInputJSON))
	if err != nil || in.Screenshot != nil {
		t.Fatalf("without screenshot: %v, screenshot = %s — absent must mean false", err, in.Screenshot)
	}
	for _, v := range []bool{true, false} {
		if _, err := parseInput(mutate(t, func(m map[string]any) { m["screenshot"] = v })); err != nil {
			t.Errorf("screenshot %v: %v", v, err)
		}
	}
	if _, err := parseInput(mutate(t, func(m map[string]any) { m["screenshot"] = true; m["read_only"] = true })); err != nil {
		t.Errorf("screenshot with read_only: %v", err)
	}
	if _, err := parseInput(mutate(t, func(m map[string]any) { m["screenshot"] = true; withSignIn(m) })); err != nil {
		t.Errorf("screenshot with sign_in: %v", err)
	}
	for name, v := range map[string]any{"null": nil, "string": "true", "number": 1, "object": map[string]any{}} {
		t.Run(name, func(t *testing.T) {
			_, err := parseInput(mutate(t, func(m map[string]any) { m["screenshot"] = v }))
			if err == nil || !strings.Contains(err.Error(), "screenshot must be true or false") {
				t.Fatalf("parseInput: %v, want an error naming screenshot", err)
			}
		})
	}
}

func TestRunRefusesAnInvalidScreenshot(t *testing.T) {
	h := newHarness(t, `exit 0`)
	if err := os.WriteFile(filepath.Join(h.cfg.ioDir, "input.json"), mutate(t, func(m map[string]any) { m["screenshot"] = "true" }), 0o644); err != nil {
		t.Fatal(err)
	}
	code, r := h.run()
	wantError(t, code, r, "invalid input", "screenshot")
}

// testJPEG is the start of a JPEG file: SOI, then an APP0 marker.
var testJPEG = append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, []byte("rest of the image")...)

// withScreenshot asks for a screenshot.
func withScreenshot(m map[string]any) { m["screenshot"] = true }

// scriptWithScreenshot is a fake script that writes image (unless nil) to
// its fourth argument, then result as its result.
func scriptWithScreenshot(t *testing.T, result string, image []byte) string {
	t.Helper()
	script := scriptWriting(t, result)
	if image == nil {
		return script
	}
	path := filepath.Join(t.TempDir(), "image")
	if err := os.WriteFile(path, image, 0o644); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`cat %q > "$4"; %s`, path, script)
}

// newScreenshotHarness is a harness whose input asks for a screenshot, and
// has sign_in when signIn is true.
func newScreenshotHarness(t *testing.T, script string, signIn bool) *harness {
	t.Helper()
	h := newHarness(t, script)
	in := mutate(t, func(m map[string]any) {
		withScreenshot(m)
		if signIn {
			withSignIn(m)
		}
	})
	if err := os.WriteFile(filepath.Join(h.cfg.ioDir, "input.json"), in, 0o644); err != nil {
		t.Fatal(err)
	}
	return h
}

// wantIO fails unless /io holds exactly names (sorted).
func (h *harness) wantIO(names ...string) {
	h.t.Helper()
	if got := h.ioEntries(); !slices.Equal(got, names) {
		h.t.Errorf("/io holds %v, want %v", got, names)
	}
}

const capturedResult = `{"schema_version":1,"status":"completed","verdict":"pass","x":"<&> é","n":1.50,"screenshot":{"width":412,"height":839}}`

func TestRunPassesTheScreenshotPath(t *testing.T) {
	h := newScreenshotHarness(t, `printf '{"schema_version":1,"status":"completed","verdict":"pass","path":"%s"}' "$4" > "$2"`, false)
	_, r := h.run()
	want := filepath.Join(h.cfg.ioDir, fmt.Sprintf(".screenshot-%d.tmp.jpg", os.Getpid()))
	if r["path"] != want {
		t.Errorf("fourth argument = %v, want %s", r["path"], want)
	}
}

func TestRunKeepsTheScreenshot(t *testing.T) {
	h := newScreenshotHarness(t, scriptWithScreenshot(t, capturedResult, testJPEG), false)
	code, _ := h.run()
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	data, err := os.ReadFile(filepath.Join(h.cfg.ioDir, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != capturedResult {
		t.Errorf("result = %s, want byte-for-byte %s", data, capturedResult)
	}
	image, err := os.ReadFile(filepath.Join(h.cfg.ioDir, "screenshot.jpg"))
	if err != nil || !bytes.Equal(image, testJPEG) {
		t.Errorf("screenshot.jpg = %q (%v), want the script's image", image, err)
	}
	h.wantIO("input.json", "result.json", "screenshot.jpg")
}

func TestRunKeepsAScreenshotOfExactly1MiB(t *testing.T) {
	image := append(slices.Clone(testJPEG), make([]byte, maxScreenshotBytes-len(testJPEG))...)
	h := newScreenshotHarness(t, scriptWithScreenshot(t, capturedResult, image), false)
	_, r := h.run()
	if r["screenshot"] == nil || r["screenshot_error"] != nil {
		t.Fatalf("result = %v, want the screenshot kept", r)
	}
	h.wantIO("input.json", "result.json", "screenshot.jpg")
}

func TestRunKeepsTheScreenshotWithSignIn(t *testing.T) {
	result := `{"schema_version":1,"status":"completed","verdict":"pass","x":"typed pa\"ss\\<wörd>","screenshot":{"width":1280,"height":800}}`
	h := newScreenshotHarness(t, scriptWithScreenshot(t, result, testJPEG), true)
	code, r := h.run()
	if code != 0 {
		t.Fatalf("exit code = %d, result %v", code, r)
	}
	if r["x"] != "typed <redacted>" {
		t.Errorf("x = %v, want the password redacted", r["x"])
	}
	if s, _ := r["screenshot"].(map[string]any); s["width"] != float64(1280) || s["height"] != float64(800) {
		t.Errorf("screenshot = %v", r["screenshot"])
	}
	h.wantIO("input.json", "result.json", "screenshot.jpg")
}

func TestRunDropsAScreenshotItCannotKeep(t *testing.T) {
	tooLarge := append(slices.Clone(testJPEG), make([]byte, maxScreenshotBytes)...)
	cases := map[string]struct {
		image []byte
		want  string
	}{
		"too large":  {tooLarge, "too large"},
		"not a JPEG": {[]byte("\x89PNG\r\n\x1a\n"), "the page could not be captured"},
		"empty":      {[]byte{}, "the page could not be captured"},
		"missing":    {nil, "the page could not be captured"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newScreenshotHarness(t, scriptWithScreenshot(t, capturedResult, tc.image), false)
			code, r := h.run()
			if code != 0 {
				t.Fatalf("exit code = %d", code)
			}
			if _, ok := r["screenshot"]; ok || r["screenshot_error"] != tc.want {
				t.Errorf("result = %v, want screenshot_error %q and no screenshot", r, tc.want)
			}
			// Without sign_in, every value is kept as the script wrote it.
			if r["x"] != "<&> é" || r["verdict"] != "pass" {
				t.Errorf("result = %v, want its other values unchanged", r)
			}
			data, _ := os.ReadFile(filepath.Join(h.cfg.ioDir, "result.json"))
			if bytes.Contains(data, []byte(redacted)) || !bytes.Contains(data, []byte(`"n":1.50`)) || !bytes.Contains(data, []byte(`"<&> é"`)) {
				t.Errorf("result = %s, want strings and numbers as the script wrote them", data)
			}
			h.wantIO("input.json", "result.json")
		})
	}
}

func TestRunDropsAScreenshotItCannotKeepWithSignIn(t *testing.T) {
	result := `{"schema_version":1,"status":"completed","verdict":"pass","x":"typed pa\"ss\\<wörd>","n":1.50,"screenshot":{"width":1280,"height":800}}`
	h := newScreenshotHarness(t, scriptWithScreenshot(t, result, []byte("not a jpeg")), true)
	code, r := h.run()
	if code != 0 {
		t.Fatalf("exit code = %d, result %v", code, r)
	}
	if r["x"] != "typed <redacted>" || r["screenshot_error"] != "the page could not be captured" || r["screenshot"] != nil {
		t.Errorf("result = %v, want it redacted with screenshot_error", r)
	}
	data, _ := os.ReadFile(filepath.Join(h.cfg.ioDir, "result.json"))
	if !bytes.Contains(data, []byte(`"n":1.50`)) {
		t.Errorf("result = %s, want numbers as written", data)
	}
	h.wantIO("input.json", "result.json")
}

func TestRunLeavesNoScreenshotWithoutTheKey(t *testing.T) {
	// The script wrote an image but its result does not say so: the result
	// is handed on as it is and the image dropped.
	for name, result := range map[string]string{
		"completed":        `{"schema_version":1,"status":"completed","verdict":"fail"}`,
		"screenshot_error": `{"schema_version":1,"status":"completed","verdict":"fail","screenshot_error":"not enough time left"}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newScreenshotHarness(t, scriptWithScreenshot(t, result, testJPEG), false)
			h.run()
			data, _ := os.ReadFile(filepath.Join(h.cfg.ioDir, "result.json"))
			if string(data) != result {
				t.Errorf("result = %s, want byte-for-byte %s", data, result)
			}
			h.wantIO("input.json", "result.json")
		})
	}
}

func TestRunErrorResultsLeaveNoScreenshot(t *testing.T) {
	cases := map[string]string{
		"script error result": scriptWithScreenshot(t, `{"schema_version":1,"status":"error","error":"boom"}`, testJPEG),
		"no result":           scriptWithScreenshot(t, `not json`, testJPEG),
		"script crashed":      `printf '\377\330\377' > "$4"; exit 3`,
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			h := newScreenshotHarness(t, script, false)
			code, r := h.run()
			wantError(t, code, r)
			if _, ok := r["screenshot"]; ok {
				t.Errorf("result = %v, want no screenshot", r)
			}
			h.wantIO("input.json", "result.json")
		})
	}
	t.Run("timeout", func(t *testing.T) {
		h := newScreenshotHarness(t, `printf '\377\330\377' > "$4"; trap '' TERM; sleep 2`, false)
		h.cfg.timeout = 300 * time.Millisecond
		code, r := h.run()
		wantError(t, code, r, "timeout_ms")
		h.wantIO("input.json", "result.json")
	})
}
