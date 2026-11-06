package qa

import (
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/config"
)

var rolesPreview = config.QAPreview{
	Repository: "1-acme/api", Kind: config.QAPreviewBackend, URL: "https://d2.cloudfront.net", HealthURL: "https://d2.cloudfront.net/healthz",
	Roles: []config.QARole{{Name: "admin", Description: "Manages every account\nand its billing"}, {Name: "member"}},
}

const signInSentence = ` A criterion that needs a signed-in request to a backend with roles is checked as a role — the one the criterion names, else the one whose description fits: call verify_api_sign_in with that role and the backend preview's URL (deployment.io signs in with the role's own email and password; you never see them), then call verify_api_request with as = that role. If the sign-in doesn't succeed (one retry at most), the criterion is not checked, with reason 'couldn't sign in as <role>' — never failed. A criterion that needs a role the backend has no test user for, a backend without roles, or an API key or a token that doesn't come from signing in, is still not checked, with reason 'needs a login'. "<redacted>" in a result stands for a secret: never try to recover it or put it in a request.`

// signInToolConfig is a run with verify_api_request the runner serves
// verify_api_sign_in to.
func signInToolConfig(t *testing.T, previews ...config.QAPreview) *config.Config {
	cfg := apiToolConfig(t, previews...)
	cfg.QAAPISignInTool = true
	return cfg
}

func TestToolsForWithTheAPISignInTool(t *testing.T) {
	withSignIn := ToolReachable + "|" + ToolRendered + "|" + ToolAPIRequest + "|" + ToolAPISignIn
	apiOnly := ToolReachable + "|" + ToolRendered + "|" + ToolAPIRequest
	without := ToolReachable + "|" + ToolRendered
	noRequestTool := previewsConfig(t, connectedSite, rolesPreview)
	noRequestTool.QAAPISignInTool = true
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{"both tools and roles", signInToolConfig(t, connectedSite, rolesPreview), withSignIn},
		{"both tools, roles on one of two backends", signInToolConfig(t, rolesPreview, searchPreview), withSignIn},
		{"both tools, no roles", signInToolConfig(t, connectedSite, apiPreview), apiOnly},
		{"only the request tool, roles", apiToolConfig(t, connectedSite, rolesPreview), apiOnly},
		{"only the sign-in flag, roles", noRequestTool, without},
	} {
		if got := strings.Join(ToolsFor(tc.cfg), "|"); got != tc.want {
			t.Errorf("%s: tools = %s, want %s", tc.name, got, tc.want)
		}
	}
	if strings.Join(VerifyTools, "|") != without {
		t.Errorf("VerifyTools = %v", VerifyTools)
	}
}

func TestBuildWithTheAPISignInTool(t *testing.T) {
	cfg := signInToolConfig(t, connectedSite, rolesPreview, searchPreview)
	prompt, instruction := Build(cfg), Instruction(cfg)
	for _, want := range []string{
		"- 1-acme/api (backend): https://d2.cloudfront.net (health check: https://d2.cloudfront.net/healthz)\n" +
			"  Roles with a test user (sign in with verify_api_sign_in):\n" +
			"  - admin: Manages every account and its billing\n" +
			"  - member\n" +
			"- 5-acme/search (backend): https://d5.cloudfront.net (health check: https://d5.cloudfront.net/healthz)\n",
		signInSentence + " The preview database",
		apiRuleStart,
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "One that needs an API key, a token or a signed-in session") {
		t.Errorf("prompt keeps the old login sentence:\n%s", prompt)
	}
	if !strings.Contains(instruction, `"`+ReasonCloudFrontHeader+`", "`+ReasonCouldntSignIn+`"`) {
		t.Errorf("instruction is missing %q:\n%s", ReasonCouldntSignIn, instruction)
	}
}

// Without the sign-in tool in effect — no flag, no request tool, or no
// backend with roles — the run is exactly as without the roles and the flag.
func TestAPISignInToolNotInEffectChangesNothing(t *testing.T) {
	strip := func(cfg *config.Config) *config.Config {
		c := *cfg
		c.QAAPISignInTool = false
		c.QAPreviews = nil
		for _, p := range cfg.QAPreviews {
			p.Roles = nil
			c.QAPreviews = append(c.QAPreviews, p)
		}
		return &c
	}
	noRequestTool := previewsConfig(t, connectedSite, rolesPreview)
	noRequestTool.QAAPISignInTool = true
	for name, cfg := range map[string]*config.Config{
		"no flag":         apiToolConfig(t, connectedSite, rolesPreview),
		"no request tool": noRequestTool,
		"no roles":        signInToolConfig(t, connectedSite, apiPreview),
	} {
		if Build(cfg) != Build(strip(cfg)) || Instruction(cfg) != Instruction(strip(cfg)) {
			t.Errorf("%s: the roles or the flag changed the run", name)
		}
		if strings.Contains(Build(cfg), ToolAPISignIn) || strings.Contains(strings.Join(ToolsFor(cfg), "|"), ToolAPISignIn) {
			t.Errorf("%s: the run has %s", name, ToolAPISignIn)
		}
	}
}
