package qa

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/config"
)

var (
	// companionBackend is a deployed companion: an unchanged preview of a
	// backend outside the Task, deployed so the site previews can call it.
	companionBackend = config.QAPreview{Repository: "1-acme/api", Kind: config.QAPreviewBackend, URL: "https://d2.cloudfront.net", HealthURL: "https://d2.cloudfront.net/healthz", Companion: true, Deployment: "acme-api"}
	// failedCompanion is a companion whose deploy failed.
	failedCompanion = config.QAPreview{Repository: "3-acme/billing", Kind: config.QAPreviewBackend, Error: "the image build failed", Companion: true, Deployment: "acme-billing"}
	// notDeployedSite is a deployed site with variables that have no backend
	// preview to point at: one with the API it calls, one without.
	notDeployedSite = config.QAPreview{
		Repository: "0-acme/web", Kind: config.QAPreviewSite, URL: "https://d1.cloudfront.net", ConnectedBackends: []string{"1-acme/api"},
		BackendsNotDeployed: []config.QABackendNotDeployed{
			{Variable: "VITE_BILLING_URL", API: "acme-billing", Reason: "the web service is in another region"},
			{Variable: "VITE_SEARCH_URL", Reason: "no web service serves it"},
		},
	}
)

const noBackendPreviewRule = "A criterion that needs the API a variable listed under a site preview points at is not checked, with reason 'deployment.io couldn't deploy a preview of the backend this needs'."

func TestBuildListsCompanions(t *testing.T) {
	pages := companionBackend
	pages.ServesPages = true
	withRoles := companionBackend
	withRoles.Roles = []config.QARole{{Name: "admin", Description: "Manages every account"}}
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{"deployed", previewsConfig(t, connectedSite, companionBackend), "- 1-acme/api (backend, not part of this change: an unchanged preview of acme-api, deployed so the site previews can call it): https://d2.cloudfront.net (health check: https://d2.cloudfront.net/healthz)\n"},
		{"serves pages", previewsConfig(t, connectedSite, pages), "- 1-acme/api (backend, serves pages, not part of this change: an unchanged preview of acme-api, deployed so the site previews can call it): https://d2.cloudfront.net (health check: https://d2.cloudfront.net/healthz)\n"},
		{"failed", previewsConfig(t, plainSite, failedCompanion), "- 3-acme/billing (backend, not part of this change: an unchanged preview of acme-billing): failed to deploy: the image build failed\n"},
		{"roles", signInToolConfig(t, connectedSite, withRoles), "- 1-acme/api (backend, not part of this change: an unchanged preview of acme-api, deployed so the site previews can call it): https://d2.cloudfront.net (health check: https://d2.cloudfront.net/healthz)\n" +
			"  Roles with a test user (sign in with verify_api_sign_in):\n" +
			"  - admin: Manages every account\n"},
	} {
		if prompt := Build(tc.cfg); !strings.Contains(prompt, tc.want) {
			t.Errorf("%s: prompt is missing %q:\n%s", tc.name, tc.want, prompt)
		}
	}
}

// A companion is otherwise a backend like any other: the run is exactly as
// with the same backend not marked a companion, but for its line.
func TestCompanionChangesOnlyItsLine(t *testing.T) {
	plain := companionBackend
	plain.Companion, plain.Deployment = false, ""
	plainFailed := failedCompanion
	plainFailed.Companion, plainFailed.Deployment = false, ""
	cfg := previewsConfig(t, connectedSite, companionBackend, failedCompanion)
	prompt, instr := Build(cfg), Instruction(cfg)
	cfg.QAPreviews = []config.QAPreview{connectedSite, plain, plainFailed}
	if instr != Instruction(cfg) {
		t.Errorf("a companion changed the instruction:\n%s", instr)
	}
	got := strings.NewReplacer(
		"(backend, not part of this change: an unchanged preview of acme-api, deployed so the site previews can call it)", "(backend)",
		"(backend, not part of this change: an unchanged preview of acme-billing)", "(backend)",
	).Replace(prompt)
	if got != Build(cfg) {
		t.Errorf("a companion changed more than its line:\n%s", prompt)
	}
}

func TestBuildListsBackendsNotDeployedUnderTheirSite(t *testing.T) {
	site := notDeployedSite
	site.BrowserRoles = []config.QABrowserRole{{Name: "owner", Backend: "1-acme/api"}}
	prompt := Build(previewsConfig(t, site, apiPreview))
	want := "- 0-acme/web (site): https://d1.cloudfront.net — built to call the backend preview of 1-acme/api\n" +
		"  Roles to sign in as in the browser (verify_preview_rendered's as):\n" +
		"  - owner (1-acme/api)\n" +
		"  VITE_BILLING_URL still calls whatever API Preview configuration points it at: the web service is in another region (acme-billing)\n" +
		"  VITE_SEARCH_URL still calls whatever API Preview configuration points it at: no web service serves it\n" +
		"- 1-acme/api (backend)"
	if !strings.Contains(prompt, want) {
		t.Errorf("prompt is missing %q:\n%s", want, prompt)
	}
}

func TestNoBackendPreviewRuleOnlyWithBackendsNotDeployed(t *testing.T) {
	quoted := `"` + ReasonNoBackendPreview + `"`
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		want bool
	}{
		{"site with items", previewsConfig(t, notDeployedSite, apiPreview), true},
		{"site only", previewsConfig(t, func() config.QAPreview { s := notDeployedSite; s.ConnectedBackends = nil; return s }()), true},
		{"with an unconnected site", previewsConfig(t, notDeployedSite, unconnectedSite, apiPreview), true},
		{"no items", previewsConfig(t, connectedSite, apiPreview), false},
		{"companions only", previewsConfig(t, connectedSite, companionBackend, failedCompanion), false},
	} {
		prompt, instr := Build(tc.cfg), Instruction(tc.cfg)
		if got := strings.Contains(prompt, noBackendPreviewRule); got != tc.want {
			t.Errorf("%s: prompt has the rule: %v, want %v:\n%s", tc.name, got, tc.want, prompt)
		}
		if got := strings.Count(instr, quoted); got != map[bool]int{true: 1, false: 0}[tc.want] {
			t.Errorf("%s: instruction names the reason %d times:\n%s", tc.name, got, instr)
		}
	}
	// The rule comes right after the not-connected rule, and the reason right
	// after its reason.
	cfg := previewsConfig(t, notDeployedSite, unconnectedSite, apiPreview)
	if prompt := Build(cfg); !strings.Contains(prompt, "\n"+siteNotConnected+" "+noBackendPreviewRule+"\n") {
		t.Errorf("the rule is not right after the not-connected rule:\n%s", prompt)
	}
	if instr := Instruction(cfg); !strings.Contains(instr, `"`+ReasonSiteNotConnected+`", `+quoted+`, `) {
		t.Errorf("the reason is not right after the not-connected reason:\n%s", instr)
	}
	// Without the not-connected rule, it is where that rule would be.
	if prompt := Build(previewsConfig(t, notDeployedSite, apiPreview)); !strings.Contains(prompt, "\n"+noBackendPreviewRule+"\n"+needsAPIRequests) {
		t.Errorf("the rule is not where the not-connected rule would be:\n%s", prompt)
	}
}

// TestCompanionGolden pins the prompt and instruction of a run with a
// deployed companion a site is connected to
// (testdata/companion_deployed_*.golden), with a failed companion
// (testdata/companion_failed_*.golden), and with a site whose variables have
// no backend preview (testdata/companion_not_deployed_*.golden).
func TestCompanionGolden(t *testing.T) {
	deployed := previewsConfig(t, connectedSite, companionBackend)
	deployed.QASpec = specJSON(t, "The pricing page lists the plans from the API")
	failed := previewsConfig(t, connectedSite, apiPreview, failedCompanion)
	failed.QASpec = specJSON(t, "The pricing page lists the plans from the API", "The invoices page lists past invoices")
	notDeployed := previewsConfig(t, notDeployedSite, companionBackend)
	notDeployed.QASpec = specJSON(t, "The pricing page lists the plans from the API", "The invoices page lists past invoices")
	for name, cfg := range map[string]*config.Config{"deployed": deployed, "failed": failed, "not_deployed": notDeployed} {
		cfg.WorkDir = "/work"
		for kind, got := range map[string]string{"prompt": Build(cfg), "instruction": Instruction(cfg)} {
			golden := filepath.Join("testdata", "companion_"+name+"_"+kind+".golden")
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
