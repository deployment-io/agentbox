package qa

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/config"
	"github.com/deployment-io/agentbox/internal/result"
)

// The evidence rule, word for word, and the criteria rule it follows.
const (
	wantEvidenceRule = `- Each criterion may add "evidence": up to 3 check_id values of the verify_preview_rendered checks its verdict rests on, most telling first, copied exactly from those checks' results (e.g. ["c4","c7"]). A person reading the result sees each cited check's screenshot beside the criterion, so cite the checks that show it — for a failed criterion, the check that shows it failing. Leave "evidence" out when no browser check bears on the criterion.`
	wantCriteriaRule = `- "criteria" has one entry per acceptance criterion: "index" is its number in the list, "state" is "passed", "failed" or "not_checked", and "reason" says briefly what you saw.`
)

func evidenceConfig(t *testing.T, spec string) *config.Config {
	cfg := qaConfig(t, spec)
	cfg.QAEvidence = true
	return cfg
}

func TestInstructionWithEvidenceHasTheRuleAfterTheCriteriaRule(t *testing.T) {
	spec := specJSON(t, "one", "two")
	previews := previewsConfig(t, connectedSite, apiPreview)
	previews.QAEvidence = true
	for name, cfg := range map[string]*config.Config{"repositories": evidenceConfig(t, spec), "previews": previews} {
		instr := Instruction(cfg)
		if !strings.Contains(instr, wantCriteriaRule+"\n"+wantEvidenceRule+"\n") {
			t.Errorf("%s: the evidence rule is not right after the criteria rule:\n%s", name, instr)
		}
		if strings.Count(instr, wantEvidenceRule) != 1 {
			t.Errorf("%s: the evidence rule appears %d times", name, strings.Count(instr, wantEvidenceRule))
		}
	}
}

// Without criteria, in production, or without the flag, the instruction has
// no evidence rule, and the prompt never changes.
func TestInstructionWithoutEvidence(t *testing.T) {
	spec := specJSON(t, "one", "two")
	prod := productionConfig(t, spec)
	prod.QAEvidence = true
	for name, cfg := range map[string]*config.Config{
		"smoke":      evidenceConfig(t, ""),
		"production": prod,
		"no flag":    qaConfig(t, spec),
	} {
		if instr := Instruction(cfg); strings.Contains(instr, `"evidence"`) {
			t.Errorf("%s: instruction has the evidence rule", name)
		}
	}
	for _, spec := range []string{spec, ""} {
		with, without := evidenceConfig(t, spec), qaConfig(t, spec)
		without.WorkDir = with.WorkDir
		if Build(with) != Build(without) {
			t.Errorf("spec %q: the flag changed the prompt", spec)
		}
		if spec == "" && Instruction(with) != Instruction(without) {
			t.Error("the flag changed a smoke run's instruction")
		}
	}
	prodWithout := productionConfig(t, spec)
	prodWithout.WorkDir = prod.WorkDir
	if Build(prod) != Build(prodWithout) || Instruction(prod) != Instruction(prodWithout) {
		t.Error("the flag changed a production run")
	}
}

// The prompt and instruction of a preview criteria run with QA_EVIDENCE=1
// (testdata/evidence_criteria_*.golden). The prompt is preview_criteria's.
func TestBuildWithEvidenceGoldens(t *testing.T) {
	cfg := evidenceConfig(t, "[Re-run instructions]\nThe CTA should say Start free.\n[Spec]\n"+specJSON(t, "The pricing page shows three plans", "Each plan has a Sign up button"))
	cfg.WorkDir = "/work"
	for kind, got := range map[string]string{"prompt": Build(cfg), "instruction": Instruction(cfg)} {
		golden := filepath.Join("testdata", "evidence_criteria_"+kind+".golden")
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

// liftEvidenceCase lifts a trailer whose second criterion carries evidence
// and checks that the criterion's state and reason, and the rest of the
// block, survive whatever the evidence is.
func liftEvidenceCase(t *testing.T, cfg *config.Config, evidence any) result.QACriterion {
	t.Helper()
	second := map[string]any{"index": 2, "state": "failed", "reason": "the Pro plan has none"}
	if evidence != nil {
		second["evidence"] = evidence
	}
	got, _ := Lift(cfg, trailer(t, map[string]any{
		"status":       "completed",
		"criteria":     []map[string]any{{"index": 1, "state": "passed", "reason": "ok"}, second},
		"findings":     []map[string]any{{"key": "qa-x", "location": "/ (criterion 2)", "what": "w"}},
		"observations": []string{"a console error"},
	}))
	if got.Status != result.QAStatusCompleted || len(got.Findings) != 1 || len(got.Observations) != 1 {
		t.Fatalf("evidence %v: the block did not parse: %+v", evidence, got)
	}
	if c := got.Criteria[0]; c.State != result.QAStatePassed || c.Reason != "ok" || c.Evidence != nil {
		t.Errorf("evidence %v: first criterion = %+v", evidence, c)
	}
	c := got.Criteria[1]
	if c.State != result.QAStateFailed || c.Reason != "the Pro plan has none" {
		t.Errorf("evidence %v: second criterion = %+v", evidence, c)
	}
	return c
}

func TestLiftEvidence(t *testing.T) {
	cfg := evidenceConfig(t, specJSON(t, "first", "second", "third"))
	for _, tc := range []struct {
		name     string
		evidence any
		want     []string
	}{
		{"kept", []string{"c4", "c7"}, []string{"c4", "c7"}},
		{"trimmed", []string{" c4 ", "\tc7\n"}, []string{"c4", "c7"}},
		{"invalid dropped", []string{"C4", "c 4", "", "c4!", strings.Repeat("c", 17), "check_1-a", strings.Repeat("c", 16)}, []string{"check_1-a", strings.Repeat("c", 16)}},
		{"duplicates dropped", []string{"c4", "c7", " c4", "c7"}, []string{"c4", "c7"}},
		{"capped at 3", []string{"c1", "c2", "c3", "c4", "c5"}, []string{"c1", "c2", "c3"}},
		{"non-strings skipped", []any{1, "c2", nil, map[string]any{"id": "c3"}, true, []string{"c4"}, "c5"}, []string{"c2", "c5"}},
		{"single string", "c4", []string{"c4"}},
		{"single invalid string", "C4", nil},
		{"number", 4, nil},
		{"object", map[string]any{"c4": true}, nil},
		{"bool", true, nil},
		{"empty", []string{}, nil},
		{"absent", nil, nil},
	} {
		if got := liftEvidenceCase(t, cfg, tc.evidence).Evidence; fmt.Sprint(got) != fmt.Sprint(tc.want) || (tc.want == nil) != (got == nil) {
			t.Errorf("%s: evidence = %#v, want %#v", tc.name, got, tc.want)
		}
	}
	// A JSON null, which a Go nil map value can't express.
	got, _ := Lift(cfg, "<qa>\n"+`{"status":"completed","criteria":[{"index":1,"state":"failed","reason":"r","evidence":null}]}`+"\n</qa>")
	if c := got.Criteria[0]; c.State != result.QAStateFailed || c.Reason != "r" || c.Evidence != nil {
		t.Errorf("null evidence: criterion = %+v", c)
	}
	// A criterion the trailer doesn't report has none.
	got, _ = Lift(cfg, trailer(t, map[string]any{"status": "completed", "criteria": []map[string]any{{"index": 1, "state": "passed", "evidence": []string{"c1"}}}}))
	if got.Criteria[0].Evidence == nil || got.Criteria[2].Evidence != nil {
		t.Errorf("criteria = %+v", got.Criteria)
	}
}

func TestLiftWithoutEvidenceFlagReadsNoEvidence(t *testing.T) {
	spec := specJSON(t, "first", "second")
	prod := productionConfig(t, spec)
	prod.QAEvidence = true
	for name, cfg := range map[string]*config.Config{"no flag": qaConfig(t, spec), "production": prod} {
		if c := liftEvidenceCase(t, cfg, []string{"c4", "c7"}); c.Evidence != nil {
			t.Errorf("%s: evidence = %v", name, c.Evidence)
		}
	}
}

func TestQAResultJSONOmitsEmptyEvidence(t *testing.T) {
	for _, tc := range []struct {
		c    result.QACriterion
		want string
	}{
		{result.QACriterion{Criterion: "a", State: "passed", Reason: "ok"}, `{"criterion":"a","state":"passed","reason":"ok"}`},
		{result.QACriterion{Criterion: "a", State: "passed", Reason: "ok", Evidence: []string{}}, `{"criterion":"a","state":"passed","reason":"ok"}`},
		{result.QACriterion{Criterion: "a", State: "failed", Reason: "no", Evidence: []string{"c4", "c7"}}, `{"criterion":"a","state":"failed","reason":"no","evidence":["c4","c7"]}`},
	} {
		b, err := json.Marshal(tc.c)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != tc.want {
			t.Errorf("json = %s, want %s", b, tc.want)
		}
	}
}
