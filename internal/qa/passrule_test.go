package qa

import (
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/config"
)

const (
	firstReportRule = "- Each criterion ends passed, failed or not checked."
	lastCheckRule   = "- A criterion is passed only when your LAST check of it passed and showed it holds. A check that failed on your own assertion — exact text that differs only in quotes, dashes, spacing or a placeholder shown as written, such as <n> — is not a pass: loosen the assertion (shorter text, a text= selector, or read the page text) and check again. If no check of it passes, the criterion is failed when what you saw shows it doesn't hold, otherwise not checked with the reason. A passed criterion's reason says what the passing check showed."
)

// Every QA mode, with criteria and without (the smoke check), tells the agent
// a criterion is passed only when its last check passed — once, directly after
// the first reporting rule.
func TestInstructionSaysPassedNeedsLastCheckPassed(t *testing.T) {
	criteria := specJSON(t, "The pricing page shows three plans")
	configs := map[string]func(spec string) *config.Config{
		"preview": func(spec string) *config.Config { return qaConfig(t, spec) },
		"preview with QA_BACKENDS": func(spec string) *config.Config {
			cfg := qaConfig(t, spec)
			cfg.QARepositories = nil
			cfg.QABackends = []config.QABackend{deployedAPI, failedAPI}
			return cfg
		},
		"preview with QA_BACKENDS and repositories": func(spec string) *config.Config {
			cfg := qaConfig(t, spec)
			cfg.QABackends = []config.QABackend{deployedAPI, failedAPI}
			return cfg
		},
		"preview with QA_PREVIEWS": func(spec string) *config.Config {
			cfg := previewsConfig(t, connectedSite, unconnectedSite, apiPreview, failedSite, failedBackend)
			cfg.QASpec = spec
			return cfg
		},
		"production": func(spec string) *config.Config { return productionConfig(t, spec) },
	}
	for name, build := range configs {
		for _, spec := range []string{criteria, ""} {
			instr := Instruction(build(spec))
			if n := strings.Count(instr, lastCheckRule); n != 1 {
				t.Errorf("%s (spec %q): the last-check rule appears %d times:\n%s", name, spec, n, instr)
				continue
			}
			lines := strings.Split(instr, "\n")
			found := false
			for i, line := range lines {
				if strings.HasPrefix(line, firstReportRule) {
					found = true
					if i+1 >= len(lines) || lines[i+1] != lastCheckRule {
						t.Errorf("%s (spec %q): the last-check rule does not directly follow the first reporting rule:\n%s", name, spec, instr)
					}
				}
			}
			if !found {
				t.Errorf("%s (spec %q): no first reporting rule:\n%s", name, spec, instr)
			}
		}
	}
}
