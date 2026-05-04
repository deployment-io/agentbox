package agent

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/config"
)

// fakeDriver implements Driver with trivial behavior for registry tests.
type fakeDriver struct{ version string }

func (f *fakeDriver) Ensure(context.Context) error      { return nil }
func (f *fakeDriver) Binary() string                    { return "fake-agent" }
func (f *fakeDriver) BuildArgs(*config.Config) []string { return nil }
func (f *fakeDriver) DetectVersion() string             { return f.version }
func (f *fakeDriver) NewOutputParser() OutputParser     { return &fakeParser{} }
func (f *fakeDriver) AllowedHosts() []string            { return nil }

type fakeParser struct{}

func (*fakeParser) Consume(io.Reader)    {}
func (*fakeParser) State() ParsedState   { return ParsedState{} }

func TestDriverFor_Unknown(t *testing.T) {
	_, err := DriverFor("definitely-not-registered", "")
	if err == nil {
		t.Fatal("expected error for unregistered agent type")
	}
	if !strings.Contains(err.Error(), "definitely-not-registered") {
		t.Errorf("error should name the unregistered type: %v", err)
	}
}

func TestRegister_AndResolve(t *testing.T) {
	const typeName = "test-agent-register-resolve"
	Register(typeName, func(v string) Driver { return &fakeDriver{version: v} })
	defer delete(registry, typeName)

	d, err := DriverFor(typeName, "1.2.3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.DetectVersion() != "1.2.3" {
		t.Errorf("version wasn't passed through: got %q", d.DetectVersion())
	}
}

func TestRegister_DuplicatePanics(t *testing.T) {
	const typeName = "test-agent-duplicate"
	Register(typeName, func(string) Driver { return &fakeDriver{} })
	defer delete(registry, typeName)

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on duplicate Register")
		}
	}()
	Register(typeName, func(string) Driver { return &fakeDriver{} })
}

func TestHasAuthKeyword(t *testing.T) {
	positives := []string{
		"Invalid API key",
		"unauthorized access",
		"HTTP 401",
		"HTTP 429",
		"rate limit exceeded",
		"you have exceeded your quota",
		"request was throttled",
		"AccessDenied: cannot invoke model",
		"Model access not enabled in region",
	}
	for _, s := range positives {
		if !HasAuthKeyword(strings.ToLower(s)) {
			t.Errorf("HasAuthKeyword(%q) = false, want true", s)
		}
	}

	negatives := []string{
		"the code failed to compile",
		"no such file or directory",
		"syntax error on line 42",
		"",
	}
	for _, s := range negatives {
		if HasAuthKeyword(strings.ToLower(s)) {
			t.Errorf("HasAuthKeyword(%q) = true, want false", s)
		}
	}
}
