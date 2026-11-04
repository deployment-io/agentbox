package qa

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/config"
)

const apiRuleStart = "A criterion about how the API answers a request — any method, with request headers or a body — and about the response's status, headers or body is checked by calling verify_api_request with url = the backend preview's URL plus the path."

// apiToolConfig is a QA_PREVIEWS run the runner serves verify_api_request to.
func apiToolConfig(t *testing.T, previews ...config.QAPreview) *config.Config {
	cfg := previewsConfig(t, previews...)
	cfg.QAAPIRequestTool = true
	return cfg
}

func TestToolsForWithTheAPIRequestTool(t *testing.T) {
	withTool := ToolReachable + "|" + ToolRendered + "|" + ToolAPIRequest
	without := ToolReachable + "|" + ToolRendered
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{"tool and a deployed backend", apiToolConfig(t, connectedSite, apiPreview), withTool},
		{"tool and only a backend", apiToolConfig(t, apiPreview), withTool},
		{"tool, no deployed backend", apiToolConfig(t, plainSite, failedBackend), without},
		{"tool, sites only", apiToolConfig(t, plainSite), without},
		{"no tool", previewsConfig(t, connectedSite, apiPreview), without},
	} {
		if got := strings.Join(ToolsFor(tc.cfg), "|"); got != tc.want {
			t.Errorf("%s: tools = %s, want %s", tc.name, got, tc.want)
		}
	}
	// The shared list isn't changed by it.
	if strings.Join(VerifyTools, "|") != without {
		t.Errorf("VerifyTools = %v", VerifyTools)
	}
	// Only a QA_PREVIEWS run: the older preview path and production never get it.
	older := qaConfig(t, "")
	older.QAAPIRequestTool = true
	older.QABackends = []config.QABackend{deployedAPI}
	prod := productionConfig(t, "")
	prod.QAAPIRequestTool = true
	for name, cfg := range map[string]*config.Config{"QA_BACKENDS": older, "production": prod} {
		if strings.Contains(strings.Join(ToolsFor(cfg), "|"), ToolAPIRequest) || strings.Contains(Build(cfg), ToolAPIRequest) || strings.Contains(Instruction(cfg), ReasonNeedsMoreResponse) {
			t.Errorf("%s run has the API request tool", name)
		}
	}
}

func TestAPIRuleReplacesTheGETRule(t *testing.T) {
	for _, tc := range []struct {
		name     string
		previews []config.QAPreview
		which    bool
	}{
		{"one backend", []config.QAPreview{connectedSite, apiPreview}, false},
		{"one deployed backend and a failed one", []config.QAPreview{connectedSite, apiPreview, failedBackend}, false},
		{"two backends", []config.QAPreview{mixedSite, apiPreview, searchPreview}, true},
	} {
		cfg := apiToolConfig(t, tc.previews...)
		prompt, instruction := Build(cfg), Instruction(cfg)
		for _, want := range []string{
			apiRuleStart,
			"A criterion about the pages of a site preview stays with verify_preview_rendered.",
			"send a JSON body with the header Content-Type: application/json",
			"Compare JSON by meaning (the same keys and values), not by spacing or key order.",
			"Redirects are not followed: a criterion about a redirect is judged from the result's status and its Location header.",
			"its database is a throwaway one",
			`"POST /items, then GET /items lists it"`,
			"with reason 'needs a login'",
			"with reason '" + ReasonNeedsPreviewData + "'",
			"with reason '" + ReasonNeedsMoreResponse + "'",
			"Check each criterion about how the API answers a request with verify_api_request",
			"Check each other criterion — about the pages of a site preview — with verify_preview_rendered",
		} {
			if !strings.Contains(prompt, want) {
				t.Errorf("%s: prompt is missing %q:\n%s", tc.name, want, prompt)
			}
		}
		if strings.Contains(prompt, "Check each criterion with verify_preview_rendered") {
			t.Errorf("%s: prompt sends every criterion to verify_preview_rendered:\n%s", tc.name, prompt)
		}
		if got := strings.Contains(prompt, "'"+ReasonWhichBackend+"'"); got != tc.which {
			t.Errorf("%s: prompt names %q: %v", tc.name, ReasonWhichBackend, got)
		}
		reasons := `"` + ReasonNeedsMoreResponse + `", "` + ReasonNeedsPreviewData + `"`
		if tc.which {
			reasons += `, "` + ReasonWhichBackend + `"`
		}
		if !strings.Contains(instruction, reasons+", it needs a preview that failed to deploy") {
			t.Errorf("%s: instruction is missing %s:\n%s", tc.name, reasons, instruction)
		}
		for kind, text := range map[string]string{"prompt": prompt, "instruction": instruction} {
			for _, absent := range []string{ReasonNeedsAPIRequests, ReasonNeedsResponseDetail, "plain GET"} {
				if strings.Contains(text, absent) {
					t.Errorf("%s: %s has %q", tc.name, kind, absent)
				}
			}
		}
	}
}

// With the flag but no deployed backend, the run is exactly as without it.
func TestAPIRequestToolWithoutABackendChangesNothing(t *testing.T) {
	for _, previews := range [][]config.QAPreview{{plainSite}, {plainSite, failedBackend}, {unconnectedSite, failedBackend}} {
		with, without := apiToolConfig(t, previews...), previewsConfig(t, previews...)
		without.WorkDir = with.WorkDir
		if Build(with) != Build(without) || Instruction(with) != Instruction(without) {
			t.Errorf("%v: the flag changed the run", previews)
		}
	}
}

// apiRequestCases are the QA_PREVIEWS runs with verify_api_request whose prompt
// and instruction testdata/api_request_*.golden pin.
func apiRequestCases(t *testing.T) map[string]*config.Config {
	spec := specJSON(t, "POST /items with a name returns 201 and the new item", "GET /items lists the new item")
	one := apiToolConfig(t, connectedSite, apiPreview)
	one.QASpec = spec
	two := apiToolConfig(t, mixedSite, apiPreview, failedBackend, searchPreview)
	two.QASpec = spec
	cases := map[string]*config.Config{"one_backend": one, "two_backends": two}
	for _, cfg := range cases {
		cfg.WorkDir = "/work"
	}
	return cases
}

func TestBuildWithTheAPIRequestToolGoldens(t *testing.T) {
	for name, cfg := range apiRequestCases(t) {
		for kind, got := range map[string]string{"prompt": Build(cfg), "instruction": Instruction(cfg)} {
			golden := filepath.Join("testdata", "api_request_"+name+"_"+kind+".golden")
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
