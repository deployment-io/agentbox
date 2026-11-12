package qa

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/config"
)

// pagesBackend is a backend preview whose front page answered a web page.
var pagesBackend = config.QAPreview{Repository: "8-acme/app", Kind: config.QAPreviewBackend, URL: "https://d8.cloudfront.net", HealthURL: "https://d8.cloudfront.net/healthz", ServesPages: true}

// pagesBackendWithRoles is pagesBackend with roles for the API and roles of
// its own to sign in as in the browser.
var pagesBackendWithRoles = config.QAPreview{
	Repository: "8-acme/app", Kind: config.QAPreviewBackend, URL: "https://d8.cloudfront.net", HealthURL: "https://d8.cloudfront.net/healthz", ServesPages: true,
	Roles: []config.QARole{{Name: "admin", Description: "Manages every account"}},
	BrowserRoles: []config.QABrowserRole{
		{Name: "owner", Description: "Owns the workspace\nand its billing", Backend: "8-acme/app"},
		{Name: "member", Backend: "8-acme/app"},
	},
}

const (
	pagesParagraph = `A backend preview marked "serves pages" is also a web app: its front page answered with a web page. A criterion about its pages — what a page shows, a form, a link, a flow through its pages — is checked in the browser with verify_preview_rendered on its URL, as for a site preview's pages; a criterion about how its API answers a request stays with the rule above.`

	previewSignInParagraph = `A criterion about a preview's pages that needs a signed-in user is checked as one of the roles listed under that preview to sign in as in the browser: the one the criterion names, else the one whose description fits. Call verify_preview_rendered with as = that role, and backend = the backend shown beside it when the preview lists that role for more than one backend; deployment.io signs in on the app's own sign-in page with the role's email and password before the check, and you never see them. Pass the preview's front page (its URL with path /) as url and reach the page the criterion is about by clicking, not with goto: the check starts on the page the app shows after signing in, and a full page load (a goto, or any other url) can sign the browser out on a preview. The result's sign_in says how the sign-in went. If sign_in.signed_in is false, or sign_in.signed_out_on_load is true, try once more; if it still doesn't work, the criterion is not checked — with reason 'the browser refused the session cookie across preview hosts' when sign_in.refused_cookies is not empty, else 'couldn't sign in as <role>' — never failed. sign_in.failed_requests and sign_in.refused_cookies are never findings or observations: they come from signing in and from how previews are hosted, not from the change. A criterion that needs a signed-in page of a preview with no roles to sign in as in the browser is not checked, with reason 'needs a login'. "<redacted>" in a result stands for a secret: never try to recover it or put it in a step.`

	pagesSmokeText = `This Task has no acceptance criteria. Run a SMOKE CHECK instead: each site preview, and each backend preview marked "serves pages", loads at its URL with no page errors and no failed first-party requests; each other backend preview answers its health check. A backend preview not marked "serves pages" is an API: its health check is its whole smoke check. Don't open its URL or any other path of it, in the browser or with a request, and what another path of it answers (such as a 404 at /) is not a finding. Say in your summary that this was a smoke check.` + "\n"
	pagesSmokeRule = `- There are no acceptance criteria, so "criteria" is []. A smoke check that finds a page error or a failed first-party request on a site preview, or on a backend preview marked "serves pages", reports it as a finding with location = the preview URL path. The smoke check of a backend preview not marked "serves pages" is its health check alone: what any other path of it answers is neither a finding nor an observation.`

	sitePageStep  = "Check each other criterion — about the pages of a site preview — with verify_preview_rendered: goto the page"
	pagesPageStep = `Check each other criterion — about the pages of a site preview or of a backend preview marked "serves pages" — with verify_preview_rendered: goto the page`
)

func TestBuildListsABackendThatServesPagesWithItsBrowserRoles(t *testing.T) {
	prompt := Build(signInToolConfig(t, connectedSite, apiPreview, pagesBackendWithRoles))
	want := "- 1-acme/api (backend): https://d2.cloudfront.net (health check: https://d2.cloudfront.net/healthz)\n" +
		"- 8-acme/app (backend, serves pages): https://d8.cloudfront.net (health check: https://d8.cloudfront.net/healthz)\n" +
		"  Roles with a test user (sign in with verify_api_sign_in):\n" +
		"  - admin: Manages every account\n" +
		"  Roles to sign in as in the browser (verify_preview_rendered's as):\n" +
		"  - owner (8-acme/app): Owns the workspace and its billing\n" +
		"  - member (8-acme/app)\n"
	if !strings.Contains(prompt, want) {
		t.Errorf("prompt is missing %q:\n%s", want, prompt)
	}
	// Without verify_api_sign_in its API roles aren't listed; its browser
	// roles still are.
	prompt = Build(previewsConfig(t, pagesBackendWithRoles))
	want = "- 8-acme/app (backend, serves pages): https://d8.cloudfront.net (health check: https://d8.cloudfront.net/healthz)\n" +
		"  Roles to sign in as in the browser (verify_preview_rendered's as):\n"
	if !strings.Contains(prompt, want) {
		t.Errorf("prompt is missing %q:\n%s", want, prompt)
	}
}

func TestPagesRuleOnlyWithADeployedBackendThatServesPages(t *testing.T) {
	failedPages := pagesBackend
	failedPages.URL, failedPages.HealthURL, failedPages.Error = "", "", "the image build failed"
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		want bool
	}{
		{"GET rule", previewsConfig(t, connectedSite, apiPreview, pagesBackend), true},
		{"API rule", apiToolConfig(t, connectedSite, apiPreview, pagesBackend), true},
		{"API sign-in", signInToolConfig(t, pagesBackendWithRoles), true},
		{"smoke", smokePreviews(t, pagesBackend), true},
		{"no backend serves pages", previewsConfig(t, connectedSite, apiPreview), false},
		{"API rule, no backend serves pages", apiToolConfig(t, connectedSite, apiPreview), false},
		{"failed backend", previewsConfig(t, connectedSite, apiPreview, failedPages), false},
	} {
		prompt := Build(tc.cfg)
		if got := strings.Contains(prompt, "\n"+pagesParagraph+"\n"); got != tc.want {
			t.Errorf("%s: prompt has the pages paragraph: %v, want %v:\n%s", tc.name, got, tc.want, prompt)
		}
	}
	// It comes after the GET rule or the API rule, and before the browser
	// sign-in rule.
	for name, cfg := range map[string]*config.Config{
		"GET rule": previewsConfig(t, browserSite, apiPreview, pagesBackendWithRoles),
		"API rule": apiToolConfig(t, browserSite, apiPreview, pagesBackendWithRoles),
	} {
		prompt := Build(cfg)
		rule := strings.Index(prompt, needsAPIRequests)
		if name == "API rule" {
			rule = strings.Index(prompt, apiRuleStart)
		}
		p := strings.Index(prompt, pagesParagraph)
		if rule < 0 || p < rule || strings.Index(prompt, previewSignInParagraph) < p {
			t.Errorf("%s: the paragraph is not between the rule and the browser sign-in rule:\n%s", name, prompt)
		}
	}
}

func TestBrowserSignInRuleSpeaksOfPreviewsOnlyWithBackendBrowserRoles(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{"site roles only", previewsConfig(t, browserSite, apiPreview, searchPreview, pagesBackend), browserSignInParagraph},
		{"backend roles", previewsConfig(t, connectedSite, apiPreview, pagesBackendWithRoles), previewSignInParagraph},
		{"site and backend roles", apiToolConfig(t, browserSite, apiPreview, searchPreview, pagesBackendWithRoles), previewSignInParagraph},
		{"backend roles only", signInToolConfig(t, pagesBackendWithRoles), previewSignInParagraph},
	} {
		prompt := Build(tc.cfg)
		if !strings.Contains(prompt, "\n"+tc.want+"\n") {
			t.Errorf("%s: prompt is missing the browser sign-in rule %q:\n%s", tc.name, tc.want, prompt)
		}
		if n := strings.Count(prompt, "to sign in as in the browser:"); n != 1 {
			t.Errorf("%s: prompt has %d browser sign-in rules:\n%s", tc.name, n, prompt)
		}
		instruction := Instruction(tc.cfg)
		for _, reason := range []string{ReasonCookieRefused, ReasonCouldntSignIn} {
			if n := strings.Count(instruction, `"`+reason+`"`); n != 1 {
				t.Errorf("%s: instruction names %q %d times:\n%s", tc.name, reason, n, instruction)
			}
		}
	}
	// Neither rule without browser roles.
	prompt := Build(previewsConfig(t, connectedSite, apiPreview, pagesBackend))
	if strings.Contains(prompt, "to sign in as in the browser:") {
		t.Errorf("prompt without browser roles has a browser sign-in rule:\n%s", prompt)
	}
}

func TestSmokeCheckWithABackendThatServesPages(t *testing.T) {
	for name, cfg := range map[string]*config.Config{
		"site and backend":     smokePreviews(t, connectedSite, pagesBackend),
		"with another backend": smokePreviews(t, connectedSite, apiPreview, pagesBackend),
		"backend only":         smokePreviews(t, pagesBackend),
	} {
		prompt, instr := Build(cfg), Instruction(cfg)
		if !strings.Contains(prompt, "[Acceptance criteria]\n"+pagesSmokeText) || strings.Contains(prompt, backendSmokeText) {
			t.Errorf("%s: prompt is missing the serves-pages smoke check:\n%s", name, prompt)
		}
		if !strings.Contains(instr, pagesSmokeRule+"\n") || strings.Contains(instr, backendSmokeRule) {
			t.Errorf("%s: instruction is missing the serves-pages smoke rule:\n%s", name, instr)
		}
	}
	// A backend that doesn't serve pages, or that failed, keeps agentbox#95's.
	failedPages := pagesBackend
	failedPages.URL, failedPages.HealthURL, failedPages.Error, failedPages.ServesPages = "", "", "the image build failed", false
	for name, cfg := range map[string]*config.Config{
		"no backend serves pages": smokePreviews(t, connectedSite, apiPreview),
		"failed backend":          smokePreviews(t, connectedSite, failedPages),
	} {
		prompt, instr := Build(cfg), Instruction(cfg)
		if !strings.Contains(prompt, "[Acceptance criteria]\n"+backendSmokeText) || strings.Contains(prompt, "serves pages") {
			t.Errorf("%s: prompt is missing agentbox#95's smoke check:\n%s", name, prompt)
		}
		if !strings.Contains(instr, backendSmokeRule+"\n") || strings.Contains(instr, "serves pages") {
			t.Errorf("%s: instruction is missing agentbox#95's smoke rule:\n%s", name, instr)
		}
	}
}

func TestPageStepWithABackendThatServesPages(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{"API rule, serves pages", apiToolConfig(t, connectedSite, apiPreview, pagesBackend), pagesPageStep},
		{"API sign-in, serves pages", signInToolConfig(t, pagesBackendWithRoles), pagesPageStep},
		{"API rule, no backend serves pages", apiToolConfig(t, connectedSite, apiPreview), sitePageStep},
		{"GET rule, serves pages", previewsConfig(t, connectedSite, apiPreview, pagesBackend), "Check each criterion with verify_preview_rendered: goto the page"},
	} {
		prompt := Build(tc.cfg)
		if !strings.Contains(prompt, tc.want) {
			t.Errorf("%s: prompt is missing %q:\n%s", tc.name, tc.want, prompt)
		}
		if tc.want != pagesPageStep && strings.Contains(prompt, pagesPageStep) {
			t.Errorf("%s: prompt has the serves-pages page step:\n%s", tc.name, prompt)
		}
	}
}

// TestServesPagesGolden pins the prompt and instruction of a run with a
// backend that serves pages and has browser roles
// (testdata/serves_pages_roles_*.golden), and of a smoke run with a site and
// a backend that serves pages (testdata/serves_pages_smoke_*.golden).
func TestServesPagesGolden(t *testing.T) {
	roles := signInToolConfig(t, connectedSite, apiPreview, pagesBackendWithRoles)
	roles.QASpec = specJSON(t, "A signed-in owner sees the Billing page", "GET /api/items answers 200")
	site := connectedSite
	site.ConnectedBackends = []string{pagesBackend.Repository}
	smoke := smokePreviews(t, site, pagesBackend)
	for name, cfg := range map[string]*config.Config{"roles": roles, "smoke": smoke} {
		cfg.WorkDir = "/work"
		for kind, got := range map[string]string{"prompt": Build(cfg), "instruction": Instruction(cfg)} {
			golden := filepath.Join("testdata", "serves_pages_"+name+"_"+kind+".golden")
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
				t.Errorf("%s %s differs from %s:\n%s", name, kind, golden, got)
			}
		}
	}
}
