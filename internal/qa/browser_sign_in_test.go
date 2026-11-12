package qa

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/config"
)

// browserSite is a site connected to two backends, with roles to sign in as
// in the browser: owner is a role of both.
var browserSite = config.QAPreview{
	Repository: "0-acme/web", Kind: config.QAPreviewSite, URL: "https://d1.cloudfront.net", ConnectedBackends: []string{"1-acme/api", "5-acme/search"},
	BrowserRoles: []config.QABrowserRole{
		{Name: "owner", Description: "Owns the workspace\nand its billing", Backend: "1-acme/api"},
		{Name: "member", Backend: "1-acme/api"},
		{Name: "owner", Backend: "5-acme/search"},
	},
}

const browserSignInParagraph = `A criterion about a site preview's pages that needs a signed-in user is checked as one of the roles listed under that site to sign in as in the browser: the one the criterion names, else the one whose description fits. Call verify_preview_rendered with as = that role, and backend = the backend shown beside it when the site lists that role for more than one backend; deployment.io signs in on the app's own sign-in page with the role's email and password before the check, and you never see them. Pass the site's front page (its URL with path /) as url and reach the page the criterion is about by clicking, not with goto: the check starts on the page the app shows after signing in, and a full page load (a goto, or any other url) can sign the browser out on a preview. The result's sign_in says how the sign-in went. If sign_in.signed_in is false, or sign_in.signed_out_on_load is true, try once more; if it still doesn't work, the criterion is not checked — with reason 'the browser refused the session cookie across preview hosts' when sign_in.refused_cookies is not empty, else 'couldn't sign in as <role>' — never failed. sign_in.failed_requests and sign_in.refused_cookies are never findings or observations: they come from signing in and from how previews are hosted, not from the change. A criterion that needs a signed-in page of a site with no roles to sign in as in the browser is not checked, with reason 'needs a login'. "<redacted>" in a result stands for a secret: never try to recover it or put it in a step.`

func TestBuildListsBrowserRolesUnderTheirSite(t *testing.T) {
	cfg := previewsConfig(t, browserSite, apiPreview, searchPreview)
	prompt := Build(cfg)
	want := "- 0-acme/web (site): https://d1.cloudfront.net — built to call the backend preview of 1-acme/api, 5-acme/search\n" +
		"  Roles to sign in as in the browser (verify_preview_rendered's as):\n" +
		"  - owner (1-acme/api): Owns the workspace and its billing\n" +
		"  - member (1-acme/api)\n" +
		"  - owner (5-acme/search)\n" +
		"- 1-acme/api (backend): https://d2.cloudfront.net (health check: https://d2.cloudfront.net/healthz)\n"
	if !strings.Contains(prompt, want) {
		t.Errorf("prompt is missing %q:\n%s", want, prompt)
	}
}

func TestBrowserSignInRuleOnlyWithBrowserRoles(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		want bool
	}{
		{"browser roles, GET rule", previewsConfig(t, browserSite, apiPreview, searchPreview), true},
		{"browser roles, API rule", apiToolConfig(t, browserSite, apiPreview, searchPreview), true},
		{"browser roles, API sign-in", signInToolConfig(t, browserSite, rolesPreview, searchPreview), true},
		{"no browser roles", previewsConfig(t, connectedSite, apiPreview), false},
		{"API sign-in, no browser roles", signInToolConfig(t, connectedSite, rolesPreview), false},
	} {
		prompt := Build(tc.cfg)
		if got := strings.Contains(prompt, "\n"+browserSignInParagraph+"\n"); got != tc.want {
			t.Errorf("%s: prompt has the browser sign-in paragraph: %v, want %v:\n%s", tc.name, got, tc.want, prompt)
		}
		if got := strings.Contains(Instruction(tc.cfg), ReasonCookieRefused); got != tc.want {
			t.Errorf("%s: instruction names %q: %v, want %v", tc.name, ReasonCookieRefused, got, tc.want)
		}
	}
	// It comes after the GET rule or the API rule.
	for name, cfg := range map[string]*config.Config{
		"GET rule": previewsConfig(t, browserSite, apiPreview),
		"API rule": apiToolConfig(t, browserSite, apiPreview),
	} {
		prompt := Build(cfg)
		rule := strings.Index(prompt, needsAPIRequests)
		if name == "API rule" {
			rule = strings.Index(prompt, apiRuleStart)
		}
		if p := strings.Index(prompt, browserSignInParagraph); rule < 0 || p < rule {
			t.Errorf("%s: the paragraph is not after the rule:\n%s", name, prompt)
		}
	}
}

func TestInstructionWithBrowserRolesListsEachReasonOnce(t *testing.T) {
	for name, cfg := range map[string]*config.Config{
		"no API tools":       previewsConfig(t, browserSite, apiPreview, searchPreview),
		"verify_api_request": apiToolConfig(t, browserSite, apiPreview, searchPreview),
		"verify_api_sign_in": signInToolConfig(t, browserSite, rolesPreview, searchPreview),
	} {
		instruction := Instruction(cfg)
		for _, reason := range []string{ReasonCookieRefused, ReasonCouldntSignIn} {
			if n := strings.Count(instruction, `"`+reason+`"`); n != 1 {
				t.Errorf("%s: instruction names %q %d times:\n%s", name, reason, n, instruction)
			}
		}
	}
}

// A browser role on a failed site isn't possible (QA_PREVIEWS refuses it),
// and a run on the older paths never has them: neither changes.
func TestBrowserRolesNotInEffectChangeNothing(t *testing.T) {
	failed := browserSite
	failed.URL, failed.Error = "", "the build failed"
	with := previewsConfig(t, failed, apiPreview)
	failed.BrowserRoles = nil
	without := previewsConfig(t, failed, apiPreview)
	without.WorkDir = with.WorkDir
	if Build(with) != Build(without) || Instruction(with) != Instruction(without) {
		t.Errorf("roles of a failed site changed the run")
	}
	if strings.Join(ToolsFor(previewsConfig(t, browserSite, apiPreview)), "|") != strings.Join(ToolsFor(previewsConfig(t, connectedSite, apiPreview)), "|") {
		t.Errorf("browser roles changed the tools")
	}
}

// TestBuildWithBrowserRolesGolden pins the prompt and instruction of a site +
// backend run with browser roles (testdata/browser_roles_*.golden).
func TestBuildWithBrowserRolesGolden(t *testing.T) {
	site := browserSite
	site.ConnectedBackends = []string{"1-acme/api"}
	site.BrowserRoles = site.BrowserRoles[:2]
	cfg := previewsConfig(t, site, apiPreview)
	cfg.QASpec = specJSON(t, "A signed-in owner sees the Billing page", "The pricing page lists three plans")
	cfg.WorkDir = "/work"
	for kind, got := range map[string]string{"prompt": Build(cfg), "instruction": Instruction(cfg)} {
		golden := filepath.Join("testdata", "browser_roles_"+kind+".golden")
		if os.Getenv("UPDATE_GOLDEN") == "1" {
			if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatal(err)
		}
		if got != string(want) {
			t.Errorf("%s differs from %s:\n%s", kind, golden, got)
		}
	}
}
