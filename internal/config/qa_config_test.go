package config

import (
	"strings"
	"testing"
)

// QA mode is a fourth AGENT_MODE with inputs of its own. These pin what it
// does NOT require (STEP_PROMPT, any REVIEW_* input) and what it will not run
// without (repositories to preview and the tool socket that previews them).

func qaEnv(t *testing.T, extra map[string]string) {
	t.Helper()
	env := map[string]string{
		"WORK_DIR":          t.TempDir(),
		"ANTHROPIC_API_KEY": "sk-ant-test",
		"AGENT_MODE":        ModeQA,
		"QA_REPOSITORIES":   "0-acme/website, 1-acme/docs",
		MCPSocketEnv:        "/run/agentbox/tool-rpc.sock",
	}
	for k, v := range extra {
		env[k] = v
	}
	setEnv(t, env)
}

func TestLoadQAModeLoadsItsInputs(t *testing.T) {
	qaEnv(t, map[string]string{
		"QA_SPEC":   `{"Acceptance":["the home page loads"]}`,
		"MAX_TURNS": "40",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load in qa mode: %s", err)
	}
	if cfg.Mode != ModeQA {
		t.Errorf("Mode = %q, want %q", cfg.Mode, ModeQA)
	}
	if cfg.StepPrompt != "" {
		t.Errorf("StepPrompt = %q, want empty — qa mode builds its own prompt", cfg.StepPrompt)
	}
	if want := []string{"0-acme/website", "1-acme/docs"}; strings.Join(cfg.QARepositories, "|") != strings.Join(want, "|") {
		t.Errorf("QARepositories = %q, want %q", cfg.QARepositories, want)
	}
	if cfg.QASpec != `{"Acceptance":["the home page loads"]}` {
		t.Errorf("QASpec = %q", cfg.QASpec)
	}
	if cfg.MCPSocket != "/run/agentbox/tool-rpc.sock" {
		t.Errorf("MCPSocket = %q", cfg.MCPSocket)
	}
	if cfg.MaxTurns != "40" {
		t.Errorf("MaxTurns = %q, want 40", cfg.MaxTurns)
	}
}

func TestLoadQAModeSpecIsOptional(t *testing.T) {
	qaEnv(t, nil)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %s", err)
	}
	if cfg.QASpec != "" {
		t.Errorf("QASpec = %q, want empty", cfg.QASpec)
	}
}

func TestLoadQAModeRequiresRepositories(t *testing.T) {
	for _, v := range []string{"", " , ,"} {
		qaEnv(t, map[string]string{"QA_REPOSITORIES": v})
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "QA_REPOSITORIES") {
			t.Errorf("QA_REPOSITORIES=%q: err = %v, want one naming QA_REPOSITORIES", v, err)
		}
	}
}

func TestLoadQAModeRequiresTheToolSocket(t *testing.T) {
	qaEnv(t, map[string]string{MCPSocketEnv: ""})
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), MCPSocketEnv) {
		t.Fatalf("err = %v, want one naming %s", err, MCPSocketEnv)
	}
}

func TestLoadQAModeRejectsANonLocalRepository(t *testing.T) {
	for _, repo := range []string{"../escape", "/etc", "0-acme/../../x"} {
		qaEnv(t, map[string]string{"QA_REPOSITORIES": "0-acme/website," + repo})
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), repo) {
			t.Errorf("repo %q: err = %v, want a rejection naming it", repo, err)
		}
	}
}

func TestLoadQAModeIgnoresReviewInputs(t *testing.T) {
	qaEnv(t, map[string]string{
		// Malformed on purpose: review mode would fail the load on both.
		"REVIEW_BASE_COMMITS":  "not json",
		"REVIEW_OPEN_FINDINGS": "not json",
		"REVIEW_SPEC":          "a review spec",
		"REVIEW_EFFORT":        "high",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %s", err)
	}
	if cfg.ReviewSpec != "" || cfg.ReviewBaseCommits != nil || cfg.ReviewEffort != "" {
		t.Errorf("review inputs were read in qa mode: %+v", cfg)
	}
}

func TestLoadInvalidModeNamesQA(t *testing.T) {
	setEnv(t, map[string]string{
		"STEP_PROMPT":       "do the thing",
		"WORK_DIR":          t.TempDir(),
		"ANTHROPIC_API_KEY": "sk-ant-test",
		"AGENT_MODE":        "qaa",
	})
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), `invalid AGENT_MODE "qaa": must be`) || !strings.Contains(err.Error(), `"qa"`) {
		t.Fatalf("err = %v, want the invalid-mode message listing qa", err)
	}
}

func TestLoadQAModeTargetDefaultsToPreview(t *testing.T) {
	for _, v := range []string{"", "preview", " preview "} {
		// QA_URLS is not read in preview mode, so even a malformed one loads.
		qaEnv(t, map[string]string{"QA_TARGET": v, "QA_URLS": "not=a url at all,,=x"})
		cfg, err := Load()
		if err != nil {
			t.Fatalf("QA_TARGET=%q: Load: %s", v, err)
		}
		if cfg.QATarget != QATargetPreview {
			t.Errorf("QA_TARGET=%q: QATarget = %q, want %q", v, cfg.QATarget, QATargetPreview)
		}
		if cfg.QAURLs != nil {
			t.Errorf("QA_TARGET=%q: QAURLs = %v, want none — QA_URLS is not read in preview mode", v, cfg.QAURLs)
		}
	}
}

func TestLoadQAModeRejectsAnUnknownTarget(t *testing.T) {
	for _, v := range []string{"prod", "staging", "Production"} {
		qaEnv(t, map[string]string{"QA_TARGET": v})
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "QA_TARGET") || !strings.Contains(err.Error(), v) {
			t.Errorf("QA_TARGET=%q: err = %v, want one naming QA_TARGET and the value", v, err)
		}
	}
}

func TestLoadQAModeProductionLoadsTheURLs(t *testing.T) {
	qaEnv(t, map[string]string{
		"QA_TARGET": "production",
		"QA_URLS":   " 1-acme/docs=https://docs.acme.example/ , 0-acme/website=https://acme.example:443",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %s", err)
	}
	if cfg.QATarget != QATargetProduction {
		t.Errorf("QATarget = %q", cfg.QATarget)
	}
	want := map[string]string{"0-acme/website": "https://acme.example:443", "1-acme/docs": "https://docs.acme.example/"}
	if len(cfg.QAURLs) != len(want) {
		t.Fatalf("QAURLs = %v, want %v", cfg.QAURLs, want)
	}
	for repo, u := range want {
		if cfg.QAURLs[repo] != u {
			t.Errorf("QAURLs[%q] = %q, want %q", repo, cfg.QAURLs[repo], u)
		}
	}
}

func TestLoadQAModeProductionRequiresQAURLs(t *testing.T) {
	qaEnv(t, map[string]string{"QA_TARGET": "production"})
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "QA_URLS") {
		t.Fatalf("err = %v, want one naming QA_URLS", err)
	}
}

func TestLoadQAModeProductionRejectsBadURLEntries(t *testing.T) {
	const site = "0-acme/website=https://acme.example"
	const docs = "1-acme/docs=https://docs.acme.example"
	for _, tc := range []struct{ urls, names string }{
		{site, "1-acme/docs"}, // missing
		{site + "," + docs + ",2-acme/blog=https://b.example", "2-acme/blog"},                                 // extra
		{site + "," + docs + ",0-acme/website=https://other.example", "0-acme/website=https://other.example"}, // duplicated
		{site + ",1-acme/docs/=https://docs.acme.example", "1-acme/docs/"},                                    // not matched exactly
		{site + ",1-acme/docs=http://docs.acme.example", "1-acme/docs=http://docs.acme.example"},              // not https
		{site + ",1-acme/docs=https://u:p@docs.acme.example", "1-acme/docs=https://u:p@docs.acme.example"},    // user info
		{site + ",1-acme/docs=https://u@docs.acme.example", "1-acme/docs=https://u@docs.acme.example"},        // user info
		{site + ",1-acme/docs=https://docs.acme.example:8443", "1-acme/docs=https://docs.acme.example:8443"},  // another port
		{site + ",1-acme/docs=https://", "1-acme/docs=https://"},                                              // no host
		{site + ",1-acme/docs", "1-acme/docs"},                                                                // no url
	} {
		qaEnv(t, map[string]string{"QA_TARGET": "production", "QA_URLS": tc.urls})
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "QA_URLS") || !strings.Contains(err.Error(), tc.names) {
			t.Errorf("QA_URLS=%q: err = %v, want one naming QA_URLS and %q", tc.urls, err, tc.names)
		}
	}
}
