package qa

import (
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/config"
)

var (
	connectedSite   = config.QAPreview{Repository: "0-acme/web", Kind: config.QAPreviewSite, URL: "https://d1.cloudfront.net", ConnectedBackends: []string{"1-acme/api"}}
	unconnectedSite = config.QAPreview{Repository: "2-acme/admin", Kind: config.QAPreviewSite, URL: "https://d3.cloudfront.net", UnconnectedBackends: []string{"1-acme/api"}}
	mixedSite       = config.QAPreview{Repository: "4-acme/shop", Kind: config.QAPreviewSite, URL: "https://d4.cloudfront.net", ConnectedBackends: []string{"1-acme/api"}, UnconnectedBackends: []string{"3-acme/billing", "5-acme/search"}}
	plainSite       = config.QAPreview{Repository: "6-acme/docs", Kind: config.QAPreviewSite, URL: "https://d6.cloudfront.net"}
	apiPreview      = config.QAPreview{Repository: "1-acme/api", Kind: config.QAPreviewBackend, URL: "https://d2.cloudfront.net", HealthURL: "https://d2.cloudfront.net/healthz"}
	failedSite      = config.QAPreview{Repository: "7-acme/blog", Kind: config.QAPreviewSite, Error: "the build failed: npm ERR! missing script: build"}
	failedBackend   = config.QAPreview{Repository: "3-acme/billing", Kind: config.QAPreviewBackend, Error: "the image build failed"}
)

const (
	siteStep         = "Confirm each site preview with verify_preview_reachable on its URL."
	siteNotConnected = "A criterion that needs a frontend to use a backend preview it is NOT connected to is not checked, with reason 'this site's preview isn't connected to that backend preview'."
	needsAPIRequests = "A criterion that needs requests to the API beyond its health check is not checked, with reason 'needs HTTP requests to the API'."
)

func previewsConfig(t *testing.T, previews ...config.QAPreview) *config.Config {
	cfg := qaConfig(t, specJSON(t, "one"))
	cfg.QARepositories = nil
	cfg.QAPreviews = previews
	return cfg
}

func TestBuildWithPreviewsListsEachEntryForm(t *testing.T) {
	cfg := previewsConfig(t, connectedSite, unconnectedSite, mixedSite, plainSite, apiPreview, failedSite, failedBackend)
	prompt := Build(cfg)
	want := "[Previews — deployed by deployment.io from this change]\n" +
		"- 0-acme/web (site): https://d1.cloudfront.net — built to call the backend preview of 1-acme/api\n" +
		"- 2-acme/admin (site): https://d3.cloudfront.net — NOT connected to the backend preview of 1-acme/api: it calls whatever API Preview configuration points it at\n" +
		"- 4-acme/shop (site): https://d4.cloudfront.net — built to call the backend preview of 1-acme/api — NOT connected to the backend preview of 3-acme/billing, 5-acme/search: it calls whatever API Preview configuration points it at\n" +
		"- 6-acme/docs (site): https://d6.cloudfront.net\n" +
		"- 1-acme/api (backend): https://d2.cloudfront.net (health check: https://d2.cloudfront.net/healthz)\n" +
		"- 7-acme/blog (site): failed to deploy: the build failed: npm ERR! missing script: build\n" +
		"- 3-acme/billing (backend): failed to deploy: the image build failed\n" +
		siteNotConnected + " " + needsAPIRequests + "\n"
	if !strings.Contains(prompt, want) {
		t.Errorf("prompt is missing\n%s\nin:\n%s", want, prompt)
	}
	for _, want := range []string{
		"You are QA for a change to a web application. You are checking a change, not making one: do not try to edit, build or fix anything.",
		"All you have is the previews listed below, which deployment.io deployed from this change, and the deploy facts in " + cfg.WorkDir + "/context (read only what you need, e.g. services.json, and nothing else). You never deploy anything yourself.\n",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q:\n%s", want, prompt)
		}
	}
}

func TestBuildWithPreviewsNeverDeploys(t *testing.T) {
	cfg := previewsConfig(t, connectedSite, unconnectedSite, apiPreview, failedSite)
	for kind, text := range map[string]string{"prompt": Build(cfg), "instruction": Instruction(cfg)} {
		for _, absent := range []string{ToolDeploy, "Deploy a preview", "[Repositories to preview]", "[Backend previews", ReasonFrontendNotConnected, "you deploy"} {
			if strings.Contains(text, absent) {
				t.Errorf("%s has %q:\n%s", kind, absent, text)
			}
		}
	}
}

func TestBuildWithPreviewsReasons(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		previews                   []config.QAPreview
		siteNotConnected, needsAPI bool
	}{
		{"connected site and backend", []config.QAPreview{connectedSite, apiPreview}, false, true},
		{"unconnected site and backend", []config.QAPreview{unconnectedSite, apiPreview}, true, true},
		{"sites only", []config.QAPreview{plainSite, failedSite}, false, false},
		{"failed backend only", []config.QAPreview{plainSite, failedBackend}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := previewsConfig(t, tc.previews...)
			prompt, instr := Build(cfg), Instruction(cfg)
			for kind, text := range map[string]string{"prompt": prompt, "instruction": instr} {
				if got := strings.Contains(text, ReasonSiteNotConnected); got != tc.siteNotConnected {
					t.Errorf("%s has %q: %v, want %v", kind, ReasonSiteNotConnected, got, tc.siteNotConnected)
				}
				if got := strings.Contains(text, ReasonNeedsAPIRequests); got != tc.needsAPI {
					t.Errorf("%s has %q: %v, want %v", kind, ReasonNeedsAPIRequests, got, tc.needsAPI)
				}
				if strings.Contains(text, ReasonFrontendNotConnected) {
					t.Errorf("%s has %q", kind, ReasonFrontendNotConnected)
				}
			}
			if got := strings.Contains(prompt, siteNotConnected); got != tc.siteNotConnected {
				t.Errorf("prompt has the not-connected rule: %v, want %v", got, tc.siteNotConnected)
			}
			if got := strings.Contains(prompt, needsAPIRequests); got != tc.needsAPI {
				t.Errorf("prompt has the API-requests rule: %v, want %v", got, tc.needsAPI)
			}
			subject := "a change to a static site."
			if tc.needsAPI {
				subject = "a change to a web application."
			}
			if !strings.Contains(prompt, subject) {
				t.Errorf("prompt is not about %q:\n%s", subject, prompt)
			}
		})
	}
}

func TestBuildWithPreviewsStepNumbering(t *testing.T) {
	for _, tc := range []struct {
		name     string
		previews []config.QAPreview
		want     []string
		absent   []string
	}{
		{"sites only", []config.QAPreview{connectedSite, failedBackend}, []string{
			"[How to check]\n1. " + siteStep + "\n2. Check each criterion with verify_preview_rendered",
			"3. The browser opens pages at desktop size", "5. Requests to private",
		}, []string{backendStep}},
		{"backends only", []config.QAPreview{apiPreview, failedSite}, []string{
			"[How to check]\n1. " + backendStep + "\n2. Check each criterion with verify_preview_rendered",
			"3. The browser opens pages at desktop size", "5. Requests to private",
		}, []string{siteStep}},
		{"both", []config.QAPreview{connectedSite, apiPreview}, []string{
			"[How to check]\n1. " + siteStep + "\n2. " + backendStep + "\n3. Check each criterion with verify_preview_rendered",
			"4. The browser opens pages at desktop size", "6. Requests to private",
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prompt := Build(previewsConfig(t, tc.previews...))
			for _, want := range tc.want {
				if !strings.Contains(prompt, want) {
					t.Errorf("prompt is missing %q:\n%s", want, prompt)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(prompt, absent) {
					t.Errorf("prompt has %q:\n%s", absent, prompt)
				}
			}
		})
	}
}

func TestInstructionWithPreviews(t *testing.T) {
	instr := Instruction(previewsConfig(t, unconnectedSite, apiPreview, failedSite))
	for _, want := range []string{
		`not checked ALWAYS says why: it needs a login, it is not observable on a preview (an email, a webhook), "this site's preview isn't connected to that backend preview", "needs HTTP requests to the API", it needs a preview that failed to deploy, you couldn't find the page, or a preview failed to load.`,
		"\n- A preview that failed to deploy is never a finding and never a failed criterion. Each criterion that needs it is not checked, with a reason that says that preview failed to deploy.\n",
		`- No preview that loads at all, or a browser that can't run, means "status": "couldnt_run"`,
		"<qa>", "</qa>", `"observations"`,
	} {
		if !strings.Contains(instr, want) {
			t.Errorf("instruction is missing %q:\n%s", want, instr)
		}
	}
	plain := Instruction(previewsConfig(t, plainSite))
	if want := `it needs a login, it is not observable on a preview (an email, a webhook), it needs a preview that failed to deploy,`; !strings.Contains(plain, want) {
		t.Errorf("sites-only instruction is missing %q:\n%s", want, plain)
	}
}

func TestToolsForWithPreviews(t *testing.T) {
	if got := ToolsFor(previewsConfig(t, plainSite)); strings.Join(got, "|") != ToolReachable+"|"+ToolRendered {
		t.Errorf("tools with QA_PREVIEWS = %q", got)
	}
}
