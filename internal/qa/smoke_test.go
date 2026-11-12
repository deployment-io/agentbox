package qa

import (
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/config"
)

const (
	backendSmokeText = "This Task has no acceptance criteria. Run a SMOKE CHECK instead: each site preview loads with no page errors and no failed first-party requests, and each backend preview answers its health check. A backend preview is an API, not a site: its health check is its whole smoke check. Don't open a backend preview's URL or any other path of it, in the browser or with a request, and what another path of it answers (such as a 404 at /) is not a finding. Say in your summary that this was a smoke check.\n"
	backendSmokeRule = `- There are no acceptance criteria, so "criteria" is []. A smoke check that finds a page error or a failed first-party request on a site preview reports it as a finding with location = the preview URL path. A backend preview's smoke check is its health check alone: what any other path of it answers is neither a finding nor an observation.`
	siteSmokeText    = "This Task has no acceptance criteria. Run a SMOKE CHECK instead: each preview loads with no page errors and no failed first-party requests. Say in your summary that this was a smoke check.\n"
	siteSmokeRule    = `- There are no acceptance criteria, so "criteria" is []. A smoke check that finds a page error or a failed first-party request reports it as a finding with location = the preview URL path.`
)

func TestBuildWithoutCriteriaSmokeChecksBackendsOnTheirHealthCheck(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  func(t *testing.T) *config.Config
	}{
		{"previews: site and deployed backend", func(t *testing.T) *config.Config {
			return smokePreviews(t, connectedSite, apiPreview)
		}},
		{"previews: backends only", func(t *testing.T) *config.Config {
			return smokePreviews(t, apiPreview)
		}},
		{"previews: failed backend only", func(t *testing.T) *config.Config {
			return smokePreviews(t, failedBackend)
		}},
		{"backends only", func(t *testing.T) *config.Config {
			cfg := qaConfig(t, "")
			cfg.QARepositories = nil
			cfg.QABackends = []config.QABackend{deployedAPI}
			return cfg
		}},
		{"repositories and backends", func(t *testing.T) *config.Config {
			cfg := qaConfig(t, "")
			cfg.QABackends = []config.QABackend{deployedAPI, failedAPI}
			return cfg
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg(t)
			prompt := Build(cfg)
			if !strings.Contains(prompt, "[Acceptance criteria]\n"+backendSmokeText) {
				t.Errorf("prompt is missing the backend smoke check:\n%s", prompt)
			}
			if strings.Contains(prompt, "each preview loads") {
				t.Errorf("prompt has the site-only smoke check:\n%s", prompt)
			}
			instr := Instruction(cfg)
			if !strings.Contains(instr, backendSmokeRule+"\n") {
				t.Errorf("instruction is missing the backend smoke rule:\n%s", instr)
			}
			if strings.Contains(instr, siteSmokeRule) {
				t.Errorf("instruction has the site-only smoke rule:\n%s", instr)
			}
		})
	}
}

func TestBuildWithoutBackendsKeepsTheSmokeCheck(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		text string
		rule string
	}{
		{"static", qaConfig(t, ""), siteSmokeText, siteSmokeRule},
		{"previews: sites only", smokePreviews(t, plainSite, failedSite), siteSmokeText, siteSmokeRule},
		{"production", productionWithBackends(t), siteSmokeText, strings.Replace(siteSmokeRule, "the preview URL path", "the production URL path", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prompt, instr := Build(tc.cfg), Instruction(tc.cfg)
			if strings.Contains(prompt, "backend preview answers its health check") || strings.Contains(instr, "health check alone") {
				t.Errorf("a run without backend previews has the backend smoke check:\n%s\n%s", prompt, instr)
			}
			if !strings.Contains(prompt, tc.text) {
				t.Errorf("prompt is missing %q:\n%s", tc.text, prompt)
			}
			if !strings.Contains(instr, tc.rule+"\n") {
				t.Errorf("instruction is missing %q:\n%s", tc.rule, instr)
			}
		})
	}
}

func TestBuildWithCriteriaAndBackendsHasNoSmokeCheck(t *testing.T) {
	cfg := previewsConfig(t, connectedSite, apiPreview)
	if prompt := Build(cfg); strings.Contains(prompt, "SMOKE CHECK") {
		t.Errorf("prompt with criteria has a smoke check:\n%s", prompt)
	}
	if instr := Instruction(cfg); strings.Contains(instr, "health check alone") || strings.Contains(instr, `"criteria" is []`) {
		t.Errorf("instruction with criteria has a smoke rule:\n%s", instr)
	}
}

func smokePreviews(t *testing.T, previews ...config.QAPreview) *config.Config {
	cfg := previewsConfig(t, previews...)
	cfg.QASpec = ""
	return cfg
}

// productionWithBackends is a production run that a backend would make a
// backend smoke run if it weren't production.
func productionWithBackends(t *testing.T) *config.Config {
	cfg := productionConfig(t, "")
	cfg.QABackends = []config.QABackend{deployedAPI}
	return cfg
}
