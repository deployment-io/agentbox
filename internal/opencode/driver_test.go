package opencode

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/agent"
	"github.com/deployment-io/agentbox/internal/config"
)

func TestRegistered_Opencode(t *testing.T) {
	d, err := agent.DriverFor("opencode", "")
	if err != nil {
		t.Fatalf("opencode should be registered: %v", err)
	}
	if d.Binary() != "opencode" {
		t.Errorf("Binary() = %q, want opencode", d.Binary())
	}
}

// stubOpencodeOnPath installs a fake `opencode` executable so Ensure's
// LookPath succeeds and the npm-install branch is skipped.
func stubOpencodeOnPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte("#!/bin/sh\necho stub\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestEnsure_WritesAutonomyConfig pins the autonomy contract: Ensure must write
// a config granting "permission":"allow" and point OPENCODE_CONFIG at it, so a
// headless run never blocks on a tool-approval prompt.
func TestEnsure_WritesAutonomyConfig(t *testing.T) {
	stubOpencodeOnPath(t)
	t.Setenv("OPENCODE_CONFIG", "")

	if err := (&Driver{}).Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	path := os.Getenv("OPENCODE_CONFIG")
	if path == "" {
		t.Fatal("OPENCODE_CONFIG should be set after Ensure")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("config file not written: %v", err)
	}
	if !strings.Contains(string(b), `"permission":"allow"`) {
		t.Errorf("config = %s, want it to grant permission:allow", b)
	}
}

func TestBuildArgs_Minimal(t *testing.T) {
	args := (&Driver{}).BuildArgs(&config.Config{StepPrompt: "hello"})

	want := []string{"run", "--format", "json"}
	for i, w := range want {
		if i >= len(args) || args[i] != w {
			t.Errorf("args[%d] = %q, want %q", i, argOrMissing(args, i), w)
		}
	}
	if slices.Contains(args, "--model") {
		t.Error("--model should not be present when Model is empty")
	}
	// The prompt is last and carries the user prompt plus the final-message
	// instruction (opencode run has no separate system-prompt flag here).
	last := args[len(args)-1]
	if !strings.HasPrefix(last, "hello") {
		t.Errorf("last arg should start with the prompt, got %q", last)
	}
	if !strings.Contains(last, "<pr_title>") {
		t.Error("prompt arg should include the final-message instruction")
	}
}

func TestBuildArgs_WithModel(t *testing.T) {
	args := (&Driver{}).BuildArgs(&config.Config{StepPrompt: "hi", Model: "anthropic/claude-sonnet-4-6"})
	assertFollowedBy(t, args, "--model", "anthropic/claude-sonnet-4-6")
	assertFollowedBy(t, args, "--format", "json")
}

func TestAllowedHosts_StaticInfra(t *testing.T) {
	t.Setenv("MODEL", "")
	hosts := (&Driver{}).AllowedHosts()
	for _, h := range []string{"registry.npmjs.org", "models.dev"} {
		if !slices.Contains(hosts, h) {
			t.Errorf("AllowedHosts should include %q, got %v", h, hosts)
		}
	}
}

// BOTH registry hosts must be allowed, because which one opencode fetches
// depends on its version — models.dev on older releases, models.opencode.ai on
// 1.18.x.
//
// A live 1.18.16 run failed with `denied:models.opencode.ai` while models.dev
// sat in the list unused. This is not a partial degradation: opencode cannot
// resolve a model id without the registry, so a missing host fails EVERY
// opencode run whatever the provider. And the version that decides which host
// is used is pinned in the Dockerfile, so a version bump alone can break it —
// which is why both are pinned here rather than tracking "the current one".
func TestAllowedHosts_BothRegistryHostsAllowed(t *testing.T) {
	t.Setenv("MODEL", "")
	hosts := (&Driver{}).AllowedHosts()
	for _, h := range []string{"models.dev", "models.opencode.ai"} {
		if !slices.Contains(hosts, h) {
			t.Errorf("AllowedHosts is missing registry host %q — opencode cannot resolve any model without it; got %v", h, hosts)
		}
	}
}

// TestAllowedHosts_ProviderDerivedFromModel pins the distinctive opencode
// behavior: the API host is derived from the model's provider prefix, since
// opencode is multi-provider and the allowlist can't be a constant.
func TestAllowedHosts_ProviderDerivedFromModel(t *testing.T) {
	cases := map[string]string{
		"anthropic/claude-sonnet-4-6": "api.anthropic.com",
		"openai/gpt-5.5":              "api.openai.com",
		"openrouter/meta/llama":       "openrouter.ai",
		// The provider id is "novita-ai", opencode's own key — not our
		// catalogue's "novita". Two names for one thing, and only this one
		// appears in MODEL.
		"novita-ai/qwen/qwen3-coder-next": "api.novita.ai",
	}
	for model, host := range cases {
		t.Run(model, func(t *testing.T) {
			t.Setenv("MODEL", model)
			if !slices.Contains((&Driver{}).AllowedHosts(), host) {
				t.Errorf("MODEL=%q should add host %q, got %v", model, host, (&Driver{}).AllowedHosts())
			}
		})
	}
}

func TestAllowedHosts_UnknownProviderKeepsInfra(t *testing.T) {
	t.Setenv("MODEL", "weirdprovider/foo")
	hosts := (&Driver{}).AllowedHosts()
	if !slices.Contains(hosts, "registry.npmjs.org") {
		t.Errorf("infra hosts should remain for an unknown provider, got %v", hosts)
	}
}

func argOrMissing(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	return "<missing>"
}

func assertFollowedBy(t *testing.T, args []string, flag, val string) {
	t.Helper()
	for i, a := range args {
		if a == flag {
			if i+1 < len(args) && args[i+1] == val {
				return
			}
			t.Errorf("flag %q not followed by %q (args: %v)", flag, val, args)
			return
		}
	}
	t.Errorf("flag %q not found in args %v", flag, args)
}

// ⚠️ THE HOST TABLE AND THE CREDENTIAL TABLE MUST COVER THE SAME PROVIDERS.
//
// providerHostFromModel lives here and opencodeProviderEnvKey lives in
// internal/config, deliberately unshared to avoid an import cycle. That makes
// drift easy and silent in two different directions: a provider with a
// credential entry but no host gets its egress denied by the proxy, and one
// with a host but no credential entry skips the fail-fast check and 401s
// mid-run.
//
// Neither package can see the other's unexported table, so each pins the set
// it knows and this comment names the counterpart to update.
func TestProviderHosts_CoverTheExpectedSet(t *testing.T) {
	// Keep in step with opencodeProviderEnvKey in internal/config.
	want := map[string]string{
		"anthropic":  "api.anthropic.com",
		"openai":     "api.openai.com",
		"openrouter": "openrouter.ai",
		"novita-ai":  "api.novita.ai",
		"google":     "generativelanguage.googleapis.com",
		"groq":       "api.groq.com",
		"xai":        "api.x.ai",
		"deepseek":   "api.deepseek.com",
		"mistral":    "api.mistral.ai",
	}
	for provider, host := range want {
		if got := providerHostFromModel(provider + "/some-model"); got != host {
			t.Errorf("providerHostFromModel(%q) = %q, want %q", provider, got, host)
		}
	}
	// An unknown provider contributes no host — the lenient path, covered by
	// ADDITIONAL_ALLOWED_HOSTS rather than by guessing.
	if got := providerHostFromModel("some-future-provider/m"); got != "" {
		t.Errorf("unknown provider returned host %q; it must stay lenient", got)
	}
}
