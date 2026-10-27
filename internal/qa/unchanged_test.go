package qa

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/deployment-io/agentbox/internal/config"
)

// unchangedCases are the configs without QA_PREVIEWS whose prompt and
// instruction testdata/unchanged_*.golden pin byte for byte: they were written
// by Build and Instruction at agentbox 00ca40d (v1.9.36), before QA_PREVIEWS,
// with WorkDir /work.
func unchangedCases(t *testing.T) map[string]*config.Config {
	spec := "[Re-run instructions]\nThe CTA should say Start free.\n[Spec]\n" + specJSON(t, "The pricing page shows three plans", "Each plan has a Sign up button")
	static := qaConfig(t, spec)
	backendsOnly := qaConfig(t, spec)
	backendsOnly.QARepositories = nil
	backendsOnly.QABackends = []config.QABackend{deployedAPI, failedAPI}
	both := qaConfig(t, spec)
	both.QABackends = []config.QABackend{deployedAPI, failedAPI}
	production := productionConfig(t, spec)
	productionSmoke := productionConfig(t, "")
	cases := map[string]*config.Config{
		"static":           static,
		"backends_only":    backendsOnly,
		"static_backends":  both,
		"production":       production,
		"production_smoke": productionSmoke,
	}
	for _, cfg := range cases {
		cfg.WorkDir = "/work"
	}
	return cases
}

func TestBuildWithoutQAPreviewsIsUnchanged(t *testing.T) {
	for name, cfg := range unchangedCases(t) {
		for kind, got := range map[string]string{"prompt": Build(cfg), "instruction": Instruction(cfg)} {
			golden := filepath.Join("testdata", "unchanged_"+name+"_"+kind+".golden")
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
