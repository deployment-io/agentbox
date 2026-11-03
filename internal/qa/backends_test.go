package qa

import (
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/config"
)

var (
	deployedAPI = config.QABackend{Repository: "2-acme/api", URL: "https://d1.cloudfront.net", HealthURL: "https://d1.cloudfront.net/healthz"}
	failedAPI   = config.QABackend{Repository: "3-acme/worker", Error: "The backend preview's deploy failed: the image build failed"}
)

var secondAPI = config.QABackend{Repository: "4-acme/search", URL: "https://d5.cloudfront.net", HealthURL: "https://d5.cloudfront.net/healthz"}

const backendStep = "Confirm each backend with verify_preview_reachable on its health URL. " +
	"A new backend preview takes a few minutes to answer — its service starts, and a new CloudFront distribution takes minutes to serve — so while it isn't reachable yet, call it again. " +
	"That wait is normal and says nothing about the change: don't report it as a slow backend or as an observation."

const backendRule = "The frontend previews are NOT connected to these backends: a frontend preview still calls the API that Preview configuration points it at. " +
	"A criterion that needs the frontend to use this change's backend is not checked, with reason 'the frontend preview isn't connected to the backend preview yet'.\n"

// getRulePreview is how the GET rule opens in a preview run.
const getRulePreview = "A criterion about how the API answers a plain GET to a path (a status code, a JSON body, text) is checked by calling verify_preview_rendered with url = the backend preview's URL plus that path, and no steps."

func TestBuildWithBackendsOnly(t *testing.T) {
	cfg := qaConfig(t, specJSON(t, "The API answers its health check"))
	cfg.QARepositories = nil
	cfg.QABackends = []config.QABackend{deployedAPI}
	prompt := Build(cfg)
	for _, want := range []string{
		"You are QA for a change to a web application.",
		"[Backend previews — deployed by deployment.io from this change]\n- 2-acme/api: https://d1.cloudfront.net (health check: https://d1.cloudfront.net/healthz)\n",
		backendRule + getRulePreview,
		"1. " + backendStep,
		"2. Check each criterion with verify_preview_rendered",
		"3. The browser opens pages at desktop size",
		"5. Requests to private, internal or metadata addresses",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q:\n%s", want, prompt)
		}
	}
	for _, absent := range []string{"[Repositories to preview]", "Deploy a preview of each repository", "a change to a static site", "failed to deploy"} {
		if strings.Contains(prompt, absent) {
			t.Errorf("prompt has %q:\n%s", absent, prompt)
		}
	}
}

func TestBuildWithStaticSitesAndBackends(t *testing.T) {
	cfg := qaConfig(t, specJSON(t, "one"))
	cfg.QABackends = []config.QABackend{deployedAPI, failedAPI}
	prompt := Build(cfg)
	for _, want := range []string{
		"You are QA for a change to a web application.",
		"[Repositories to preview]\n- 0-acme/website\n- 1-acme/docs\n",
		"- 2-acme/api: https://d1.cloudfront.net (health check: https://d1.cloudfront.net/healthz)\n",
		"- 3-acme/worker: failed to deploy: The backend preview's deploy failed: the image build failed\n",
		backendRule + getRulePreview,
		"1. Deploy a preview of each repository above with deploy_static_site_preview",
		"2. Confirm each preview with verify_preview_reachable on the URL it returned.",
		"3. " + backendStep,
		"4. Check each criterion with verify_preview_rendered",
		"5. The browser opens pages",
		"7. Requests to private",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q:\n%s", want, prompt)
		}
	}
	if strings.Index(prompt, "[Repositories to preview]") > strings.Index(prompt, "[Backend previews") {
		t.Error("the backends come before the repositories to preview")
	}
}

// Without backends the prompt is the static-site one (pinned byte for byte by
// TestBuildForPreviewIsUnchanged); a production run never mentions backends.
func TestBuildWithoutBackendsSaysNothingOfThem(t *testing.T) {
	for name, cfg := range map[string]*config.Config{"static": qaConfig(t, specJSON(t, "one")), "production": productionConfig(t, specJSON(t, "one"))} {
		if name == "production" {
			cfg.QABackends = []config.QABackend{deployedAPI}
		}
		for kind, text := range map[string]string{"prompt": Build(cfg), "instruction": Instruction(cfg)} {
			absents := []string{"backend preview", "web application", ReasonNeedsPreviewData, ReasonWhichBackend}
			if name == "static" {
				absents = append(absents, ReasonNeedsAPIRequests, ReasonNeedsResponseDetail, "plain GET")
			}
			for _, absent := range absents {
				if strings.Contains(text, absent) {
					t.Errorf("%s %s mentions %q", name, kind, absent)
				}
			}
		}
	}
}

func TestInstructionWithBackends(t *testing.T) {
	cfg := qaConfig(t, specJSON(t, "one"))
	cfg.QABackends = []config.QABackend{failedAPI}
	instr := Instruction(cfg)
	for _, want := range []string{
		`"the frontend preview isn't connected to the backend preview yet"`,
		`"needs an API request other than a plain GET", "needs part of the response the browser doesn't show", "needs data the preview database doesn't have", it needs a backend preview`,
		"it needs a backend preview that failed to deploy",
		"A backend preview that failed to deploy is never a finding",
		"not checked, with a reason that says the backend preview failed to deploy",
		"A preview you deploy that won't build or deploy",
	} {
		if !strings.Contains(instr, want) {
			t.Errorf("instruction is missing %q:\n%s", want, instr)
		}
	}
}

// The GET rule and its reasons: ReasonWhichBackend only with more than one
// deployed backend.
func TestBuildWithBackendsGetRule(t *testing.T) {
	for _, tc := range []struct {
		name     string
		backends []config.QABackend
		which    bool
	}{
		{"one deployed", []config.QABackend{deployedAPI}, false},
		{"one deployed, one failed", []config.QABackend{deployedAPI, failedAPI}, false},
		{"two deployed", []config.QABackend{deployedAPI, secondAPI}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := qaConfig(t, specJSON(t, "one"))
			cfg.QABackends = tc.backends
			prompt, instr := Build(cfg), Instruction(cfg)
			for _, want := range []string{getRulePreview, backendStep, "http_status", "dom_text_snippet", "a criterion expecting a 404 passes when http_status is 404", "Compare JSON by meaning", "final_url",
				"'" + ReasonNeedsAPIRequests + "'", "'needs a login'", "'" + ReasonNeedsResponseDetail + "'", "'" + ReasonNeedsPreviewData + "'"} {
				if !strings.Contains(prompt, want) {
					t.Errorf("prompt is missing %q:\n%s", want, prompt)
				}
			}
			for _, want := range []string{ReasonNeedsAPIRequests, ReasonNeedsResponseDetail, ReasonNeedsPreviewData} {
				if !strings.Contains(instr, `"`+want+`"`) {
					t.Errorf("instruction is missing %q:\n%s", want, instr)
				}
			}
			for kind, text := range map[string]string{"prompt": prompt, "instruction": instr} {
				if got := strings.Contains(text, ReasonWhichBackend); got != tc.which {
					t.Errorf("%s has %q: %v, want %v", kind, ReasonWhichBackend, got, tc.which)
				}
				if strings.Contains(text, "needs HTTP requests to the API") {
					t.Errorf("%s has the old API reason", kind)
				}
			}
		})
	}
}
