package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"
)

// Limits on input.json (docs/VERIFY_BROWSER.md). verify-browser/lib.mjs
// enforces the same ones; the runner checks them before it starts anything.
const (
	schemaVersion  = 1
	maxSteps       = 30
	maxAssertions  = 30
	maxStringChars = 2000
	minTimeoutMS   = 5000
	maxTimeoutMS   = 90000
	maxInputBytes  = 1 << 20

	minViewportWidth  = 320
	maxViewportWidth  = 2560
	minViewportHeight = 480
	maxViewportHeight = 1600

	maxEmailChars    = 320
	maxPasswordChars = 200
)

// viewportPresets are the named window sizes verify-browser/lib.mjs knows.
var viewportPresets = []string{"desktop", "tablet", "mobile"}

// input is /io/input.json.
type input struct {
	SchemaVersion int         `json:"schema_version"`
	URL           string      `json:"url"`
	AllowedHosts  []string    `json:"allowed_hosts"`
	Steps         []step      `json:"steps"`
	Assertions    []assertion `json:"assertions"`
	TimeoutMS     int         `json:"timeout_ms"`
	// Viewport is kept raw so that a non-integer size or an unknown key is
	// reported rather than silently decoded.
	Viewport json.RawMessage `json:"viewport,omitempty"`
	// ReadOnly is kept raw for the same reason: only true or false is a
	// read_only, and a null or a string must not quietly mean false.
	ReadOnly json.RawMessage `json:"read_only,omitempty"`
	// Screenshot is kept raw for the same reason as ReadOnly.
	Screenshot json.RawMessage `json:"screenshot,omitempty"`
	// SignIn is kept raw so that an unknown key, a missing field or a null
	// is reported; validate fills signIn from it.
	SignIn json.RawMessage `json:"sign_in,omitempty"`

	signIn *signIn
}

// signIn is input.json's sign_in: a test user's credentials and the page to
// sign in on.
type signIn struct {
	URL      string
	Email    string
	Password string
}

// secret is what is redacted from every result file: the sign-in's password,
// or "" when the check has no sign_in.
func (in *input) secret() string {
	if in == nil || in.signIn == nil {
		return ""
	}
	return in.signIn.Password
}

// rawSecret is sign_in.password from input.json that did not validate, or ""
// when there is none, so that an invalid-input error never carries it.
func rawSecret(data []byte) string {
	var raw struct {
		SignIn struct {
			Password any `json:"password"`
		} `json:"sign_in"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return ""
	}
	if s, ok := raw.SignIn.Password.(string); ok {
		return s
	}
	return ""
}

type step struct {
	Action   string  `json:"action"`
	URL      string  `json:"url,omitempty"`
	Selector *string `json:"selector,omitempty"`
	Value    *string `json:"value,omitempty"`
	Key      string  `json:"key,omitempty"`
	Text     *string `json:"text,omitempty"`
}

type assertion struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Selector string `json:"selector,omitempty"`
	Path     string `json:"path,omitempty"`
}

// parseInput decodes and validates input.json. The error says what is wrong
// and becomes the result's error.
func parseInput(data []byte) (*input, error) {
	if len(data) > maxInputBytes {
		return nil, fmt.Errorf("input.json is larger than %d bytes", maxInputBytes)
	}
	var in input
	if err := json.Unmarshal(data, &in); err != nil {
		return nil, fmt.Errorf("input.json is not valid JSON: %w", err)
	}
	if err := in.validate(); err != nil {
		return nil, err
	}
	return &in, nil
}

func (in *input) validate() error {
	if in.SchemaVersion != schemaVersion {
		return fmt.Errorf("schema_version must be %d, got %d", schemaVersion, in.SchemaVersion)
	}
	if in.TimeoutMS < minTimeoutMS || in.TimeoutMS > maxTimeoutMS {
		return fmt.Errorf("timeout_ms must be between %d and %d, got %d", minTimeoutMS, maxTimeoutMS, in.TimeoutMS)
	}
	if err := in.validateHosts(); err != nil {
		return err
	}
	if err := checkString("url", in.URL, false); err != nil {
		return err
	}
	if !in.allowedURL(in.URL) {
		return fmt.Errorf("url must be an https URL on a host in allowed_hosts, got %q", in.URL)
	}
	if len(in.Steps) > maxSteps {
		return fmt.Errorf("at most %d steps, got %d", maxSteps, len(in.Steps))
	}
	for i, s := range in.Steps {
		if err := in.validateStep(s); err != nil {
			return fmt.Errorf("steps[%d]: %w", i, err)
		}
	}
	if len(in.Assertions) > maxAssertions {
		return fmt.Errorf("at most %d assertions, got %d", maxAssertions, len(in.Assertions))
	}
	for i, a := range in.Assertions {
		if err := validateAssertion(a); err != nil {
			return fmt.Errorf("assertions[%d]: %w", i, err)
		}
	}
	readOnly, err := optionalBool("read_only", in.ReadOnly)
	if err != nil {
		return err
	}
	if _, err := optionalBool("screenshot", in.Screenshot); err != nil {
		return err
	}
	if in.Viewport != nil {
		if err := validateViewport(in.Viewport); err != nil {
			return err
		}
	}
	if in.SignIn != nil {
		s, err := in.validateSignIn(in.SignIn, readOnly)
		if err != nil {
			return err
		}
		in.signIn = s
	}
	return nil
}

// optionalBool decodes an optional boolean field: absent is false, and
// anything but true or false (a null, a string, a number) is an error.
func optionalBool(name string, raw json.RawMessage) (bool, error) {
	v := false
	if raw != nil && (string(raw) == "null" || json.Unmarshal(raw, &v) != nil) {
		return false, fmt.Errorf("%s must be true or false, got %s", name, raw)
	}
	return v, nil
}

// signInKeys are sign_in's fields, all required.
var signInKeys = []string{"url", "email", "password"}

// validateSignIn checks the optional sign_in: exactly {url, email,
// password}, all strings; url as the check's url; email and password
// non-empty, within their limits and on one line. Its errors name the field,
// never its value.
func (in *input) validateSignIn(raw json.RawMessage, readOnly bool) (*signIn, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, errors.New(`sign_in must be an object: {"url": ..., "email": ..., "password": ...}`)
	}
	for k := range fields {
		if !slices.Contains(signInKeys, k) {
			return nil, errors.New("sign_in takes only url, email and password")
		}
	}
	values := map[string]string{}
	for _, k := range signInKeys {
		var s *string
		if v, ok := fields[k]; !ok || json.Unmarshal(v, &s) != nil || s == nil {
			return nil, fmt.Errorf("sign_in.%s must be a string", k)
		}
		values[k] = *s
	}
	if err := checkString("sign_in.url", values["url"], false); err != nil {
		return nil, err
	}
	if !in.allowedURL(values["url"]) {
		// Not quoted: a url with user info could hold the password.
		return nil, errors.New("sign_in.url must be an https URL on a host in allowed_hosts")
	}
	if err := checkCredential("sign_in.email", values["email"], maxEmailChars); err != nil {
		return nil, err
	}
	if err := checkCredential("sign_in.password", values["password"], maxPasswordChars); err != nil {
		return nil, err
	}
	if readOnly {
		return nil, errors.New("sign_in cannot be used with read_only: a read-only check is of a live site, which has no test users")
	}
	return &signIn{URL: values["url"], Email: values["email"], Password: values["password"]}, nil
}

// lineBreaks are the characters a sign-in credential may not hold
// (verify-browser/lib.mjs refuses the same ones).
const lineBreaks = "\n\r\v\f\u0085\u2028\u2029"

func checkCredential(name, v string, max int) error {
	switch {
	case v == "":
		return fmt.Errorf("%s must be a non-empty string", name)
	case utf8.RuneCountInString(v) > max:
		return fmt.Errorf("%s is longer than %d characters", name, max)
	case strings.ContainsAny(v, lineBreaks):
		return fmt.Errorf("%s must not contain a line break", name)
	}
	return nil
}

// validateViewport checks the optional viewport: exactly {"preset"} or
// {"width", "height"}, integers within the limits.
func validateViewport(raw json.RawMessage) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return errors.New(`viewport must be an object: {"preset": ...} or {"width": ..., "height": ...}`)
	}
	_, hasPreset := fields["preset"]
	_, hasWidth := fields["width"]
	_, hasHeight := fields["height"]
	switch {
	case hasPreset && (hasWidth || hasHeight):
		return errors.New("viewport takes either preset or width and height, not both")
	case hasPreset && len(fields) == 1:
		var p string
		if err := json.Unmarshal(fields["preset"], &p); err != nil || !slices.Contains(viewportPresets, p) {
			return fmt.Errorf("viewport.preset must be desktop, tablet or mobile, got %s", fields["preset"])
		}
		return nil
	case hasWidth && hasHeight && len(fields) == 2:
		if err := checkViewportSize("width", fields["width"], minViewportWidth, maxViewportWidth); err != nil {
			return err
		}
		return checkViewportSize("height", fields["height"], minViewportHeight, maxViewportHeight)
	}
	return errors.New(`viewport must be {"preset": ...} or {"width": ..., "height": ...}`)
}

func checkViewportSize(name string, raw json.RawMessage, lo, hi int) error {
	var v float64
	if err := json.Unmarshal(raw, &v); err != nil || v != math.Trunc(v) || v < float64(lo) || v > float64(hi) {
		return fmt.Errorf("viewport.%s must be an integer between %d and %d, got %s", name, lo, hi, raw)
	}
	return nil
}

func (in *input) validateHosts() error {
	if len(in.AllowedHosts) == 0 {
		return errors.New("allowed_hosts must name at least one host")
	}
	for i, h := range in.AllowedHosts {
		if err := checkString(fmt.Sprintf("allowed_hosts[%d]", i), h, false); err != nil {
			return err
		}
		if strings.ContainsAny(h, ":/@ ") {
			return fmt.Errorf("allowed_hosts[%d] must be a bare host name, got %q", i, h)
		}
	}
	return nil
}

// allowedURL is the allowed-host check: https, no user info, port 443 or
// none, and the host exactly one of allowed_hosts (case-insensitive).
func (in *input) allowedURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	if p := u.Port(); p != "" && p != "443" {
		return false
	}
	host := normalizeHost(u.Hostname())
	if host == "" {
		return false
	}
	for _, h := range in.AllowedHosts {
		if normalizeHost(h) == host {
			return true
		}
	}
	return false
}

func normalizeHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
}

func (in *input) validateStep(s step) error {
	if err := checkLengths(map[string]*string{
		"url": &s.URL, "selector": s.Selector, "value": s.Value, "key": &s.Key, "text": s.Text,
	}); err != nil {
		return err
	}
	switch s.Action {
	case "goto":
		if err := checkString("url", s.URL, false); err != nil {
			return err
		}
		if !in.allowedURL(s.URL) {
			return fmt.Errorf("goto url must be an https URL on a host in allowed_hosts, got %q", s.URL)
		}
		return nil
	case "click":
		return checkOptional("selector", s.Selector, false)
	case "fill":
		if err := checkOptional("selector", s.Selector, false); err != nil {
			return err
		}
		return checkOptional("value", s.Value, true)
	case "press":
		if err := checkOptional("selector", s.Selector, false); err != nil {
			return err
		}
		return checkString("key", s.Key, false)
	case "wait_for":
		hasSelector := s.Selector != nil && *s.Selector != ""
		hasText := s.Text != nil && *s.Text != ""
		if hasSelector == hasText {
			return errors.New("wait_for takes exactly one of selector or text")
		}
		if hasSelector {
			return checkOptional("selector", s.Selector, false)
		}
		return checkOptional("text", s.Text, false)
	case "back":
		return nil
	}
	return fmt.Errorf("unknown action %q (want goto, click, fill, press, wait_for or back)", s.Action)
}

func validateAssertion(a assertion) error {
	if err := checkLengths(map[string]*string{
		"text": &a.Text, "selector": &a.Selector, "path": &a.Path,
	}); err != nil {
		return err
	}
	switch a.Type {
	case "text_visible", "text_absent":
		return checkString("text", a.Text, false)
	case "selector_visible":
		return checkString("selector", a.Selector, false)
	case "url_path":
		if err := checkString("path", a.Path, false); err != nil {
			return err
		}
		if !strings.HasPrefix(a.Path, "/") {
			return fmt.Errorf("path must start with /, got %q", a.Path)
		}
		return nil
	}
	return fmt.Errorf("unknown type %q (want text_visible, text_absent, selector_visible or url_path)", a.Type)
}

// checkLengths applies the per-string limit to every field of a step or
// assertion, including ones its action or type doesn't use.
func checkLengths(fields map[string]*string) error {
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		if v := fields[name]; v != nil && utf8.RuneCountInString(*v) > maxStringChars {
			return fmt.Errorf("%s is longer than %d characters", name, maxStringChars)
		}
	}
	return nil
}

func checkOptional(name string, v *string, allowEmpty bool) error {
	if v == nil {
		return fmt.Errorf("%s is required", name)
	}
	return checkString(name, *v, allowEmpty)
}

func checkString(name, v string, allowEmpty bool) error {
	if !allowEmpty && strings.TrimSpace(v) == "" {
		return fmt.Errorf("%s is required", name)
	}
	if utf8.RuneCountInString(v) > maxStringChars {
		return fmt.Errorf("%s is longer than %d characters", name, maxStringChars)
	}
	return nil
}
