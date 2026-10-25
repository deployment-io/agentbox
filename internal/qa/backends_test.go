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

const backendStep = "Confirm each backend with verify_preview_reachable on its health URL. A new backend takes a few minutes to start: while it isn't reachable yet, call it again."

const backendRule = "The frontend previews are NOT connected to these backends: a frontend preview still calls the API that Preview configuration points it at. " +
	"A criterion that needs the frontend to use this change's backend is not checked, with reason 'the frontend preview isn't connected to the backend preview yet'. " +
	"A criterion that needs requests to the API beyond its health check is not checked, with reason 'needs HTTP requests to the API'."

func TestBuildWithBackendsOnly(t *testing.T) {
	cfg := qaConfig(t, specJSON(t, "The API answers its health check"))
	cfg.QARepositories = nil
	cfg.QABackends = []config.QABackend{deployedAPI}
	prompt := Build(cfg)
	for _, want := range []string{
		"You are QA for a change to a web application.",
		"[Backend previews — deployed by deployment.io from this change]\n- 2-acme/api: https://d1.cloudfront.net (health check: https://d1.cloudfront.net/healthz)\n",
		backendRule,
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
		backendRule,
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
			for _, absent := range []string{"backend preview", "web application", ReasonNeedsAPIRequests} {
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
		`"needs HTTP requests to the API"`,
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
