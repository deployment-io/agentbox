package claude

import (
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/config"
)

func qaConfig() *config.Config {
	return &config.Config{
		Mode:           config.ModeQA,
		StepPrompt:     "the QA prompt",
		QASpec:         `{"Acceptance":["the home page loads"]}`,
		QARepositories: []string{"0-acme/website"},
		MCPSocket:      "/run/agentbox/tool-rpc.sock",
		MaxTurns:       "30",
	}
}

// QA mode: the preview tools on, no edits, no shell.
func TestBuildArgsInQAMode(t *testing.T) {
	cfg := qaConfig()
	args := (&Driver{}).BuildArgs(cfg)

	if args[0] != "-p" || args[1] == cfg.StepPrompt {
		t.Errorf("the QA prompt must go on stdin, not in the args: %q", args[:2])
	}
	if got := (&Driver{}).Stdin(cfg); got != cfg.StepPrompt {
		t.Errorf("Stdin = %q, want the QA prompt", got)
	}
	for _, a := range args {
		if a == "--dangerously-skip-permissions" {
			t.Error("qa mode bypasses permissions")
		}
	}
	if i := indexOf(args, "--mcp-config"); i < 0 || i+1 >= len(args) || !strings.Contains(args[i+1], `"deployment-io"`) || !strings.Contains(args[i+1], "mcp-bridge") {
		t.Errorf("qa mode does not wire the bridge: %q", args)
	}
	if i := indexOf(args, "--max-turns"); i < 0 || i+1 >= len(args) || args[i+1] != "30" {
		t.Errorf("MAX_TURNS not honoured: %q", args)
	}
	if i := indexOf(args, "--append-system-prompt"); i < 0 || i+1 >= len(args) || !strings.Contains(args[i+1], "<qa>") || strings.Contains(args[i+1], "<verify>") {
		t.Errorf("trailer instruction is not the QA one")
	}

	i := indexOf(args, "--allowedTools")
	if i < 0 {
		t.Fatal("qa mode has no --allowedTools")
	}
	got := args[i+1:]
	want := []string{
		"Read", "Grep", "Glob",
		"mcp__deployment-io__deploy_static_site_preview",
		"mcp__deployment-io__verify_preview_reachable",
		"mcp__deployment-io__verify_preview_rendered",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("allowlist (must be last) = %q, want exactly %q", got, want)
	}
	for _, tool := range got {
		if strings.HasPrefix(tool, "Bash") || tool == "Write" || tool == "Edit" || tool == "NotebookEdit" {
			t.Errorf("qa allowlist carries %q", tool)
		}
	}
}

// Production QA: nothing to deploy, so the deploy tool is never allowed.
func TestBuildArgsInProductionQAMode(t *testing.T) {
	cfg := qaConfig()
	cfg.QATarget = config.QATargetProduction
	cfg.QAURLs = map[string]string{"0-acme/website": "https://acme.example"}
	args := (&Driver{}).BuildArgs(cfg)

	i := indexOf(args, "--allowedTools")
	if i < 0 {
		t.Fatal("production qa mode has no --allowedTools")
	}
	got := args[i+1:]
	want := []string{
		"Read", "Grep", "Glob",
		"mcp__deployment-io__verify_preview_reachable",
		"mcp__deployment-io__verify_preview_rendered",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("allowlist (must be last) = %q, want exactly %q", got, want)
	}
	for _, a := range args {
		if strings.Contains(a, "deploy_static_site_preview") {
			t.Errorf("production args carry the deploy tool: %q", a)
		}
	}
	if j := indexOf(args, "--append-system-prompt"); j < 0 || !strings.Contains(args[j+1], "production data") {
		t.Errorf("trailer instruction is not the production QA one")
	}
}

// QA_PREVIEWS: the runner deployed every preview, so only the verify tools.
func TestBuildArgsInQAModeWithPreviews(t *testing.T) {
	cfg := qaConfig()
	cfg.QARepositories = nil
	cfg.QAPreviews = []config.QAPreview{{Repository: "0-acme/website", Kind: config.QAPreviewSite, URL: "https://d1.cloudfront.net"}}
	args := (&Driver{}).BuildArgs(cfg)

	i := indexOf(args, "--allowedTools")
	if i < 0 {
		t.Fatal("qa mode with QA_PREVIEWS has no --allowedTools")
	}
	got := args[i+1:]
	want := []string{
		"Read", "Grep", "Glob",
		"mcp__deployment-io__verify_preview_reachable",
		"mcp__deployment-io__verify_preview_rendered",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("allowlist (must be last) = %q, want exactly %q", got, want)
	}
	for _, a := range args {
		if strings.Contains(a, "deploy_static_site_preview") {
			t.Errorf("args with QA_PREVIEWS carry the deploy tool: %q", a)
		}
	}
}

// QA_PREVIEWS with a deployed backend and QA_API_REQUEST_TOOL: the verify
// tools and verify_api_request.
func TestBuildArgsInQAModeWithTheAPIRequestTool(t *testing.T) {
	cfg := qaConfig()
	cfg.QARepositories = nil
	cfg.QAPreviews = []config.QAPreview{{Repository: "1-acme/api", Kind: config.QAPreviewBackend, URL: "https://d2.cloudfront.net", HealthURL: "https://d2.cloudfront.net/healthz"}}
	cfg.QAAPIRequestTool = true
	args := (&Driver{}).BuildArgs(cfg)
	i := indexOf(args, "--allowedTools")
	if i < 0 {
		t.Fatal("qa mode has no --allowedTools")
	}
	want := []string{
		"Read", "Grep", "Glob",
		"mcp__deployment-io__verify_preview_reachable",
		"mcp__deployment-io__verify_preview_rendered",
		"mcp__deployment-io__verify_api_request",
	}
	if got := args[i+1:]; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("allowlist (must be last) = %q, want exactly %q", got, want)
	}
}

// With QA_API_SIGN_IN_TOOL and a backend with roles, verify_api_sign_in joins
// the allowlist; without roles it doesn't.
func TestBuildArgsInQAModeWithTheAPISignInTool(t *testing.T) {
	backend := config.QAPreview{Repository: "1-acme/api", Kind: config.QAPreviewBackend, URL: "https://d2.cloudfront.net", HealthURL: "https://d2.cloudfront.net/healthz"}
	for _, tc := range []struct {
		name  string
		roles []config.QARole
		want  bool
	}{{"roles", []config.QARole{{Name: "admin"}}, true}, {"no roles", nil, false}} {
		cfg := qaConfig()
		cfg.QARepositories = nil
		withRoles := backend
		withRoles.Roles = tc.roles
		cfg.QAPreviews = []config.QAPreview{withRoles}
		cfg.QAAPIRequestTool = true
		cfg.QAAPISignInTool = true
		args := (&Driver{}).BuildArgs(cfg)
		i := indexOf(args, "--allowedTools")
		if i < 0 {
			t.Fatal("qa mode has no --allowedTools")
		}
		want := []string{
			"Read", "Grep", "Glob",
			"mcp__deployment-io__verify_preview_reachable",
			"mcp__deployment-io__verify_preview_rendered",
			"mcp__deployment-io__verify_api_request",
		}
		if tc.want {
			want = append(want, "mcp__deployment-io__verify_api_sign_in")
		}
		if got := args[i+1:]; strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%s: allowlist (must be last) = %q, want exactly %q", tc.name, got, want)
		}
	}
}
