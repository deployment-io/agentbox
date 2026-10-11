package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
)

// input is /io/input.json.
type input struct {
	SchemaVersion int         `json:"schema_version"`
	URL           string      `json:"url"`
	AllowedHosts  []string    `json:"allowed_hosts"`
	Steps         []step      `json:"steps"`
	Assertions    []assertion `json:"assertions"`
	TimeoutMS     int         `json:"timeout_ms"`
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
	}
	return fmt.Errorf("unknown action %q (want goto, click, fill, press or wait_for)", s.Action)
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
