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
	"github.com/deployment-io/agentbox/internal/review"
)

func specJSON(t *testing.T, acceptance ...string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"Title":      "Pricing page",
		"Goal":       "show plans",
		"Acceptance": acceptance,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCriteriaFromStructuredSpec(t *testing.T) {
	spec := specJSON(t, "The pricing page shows three plans", "  ", "Each plan has a\n  Sign up   button")
	got, instructions := Criteria(spec)
	want := []string{"The pricing page shows three plans", "Each plan has a Sign up button"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("Criteria = %q, want %q", got, want)
	}
	if instructions != "" {
		t.Errorf("instructions = %q, want none", instructions)
	}
}

func TestCriteriaKeysAreCaseInsensitiveLikeTheRunner(t *testing.T) {
	got, _ := Criteria(`{"acceptance":["one"]}`)
	if len(got) != 1 || got[0] != "one" {
		t.Errorf("Criteria = %q, want [one]", got)
	}
}

func TestCriteriaFromProseIsNone(t *testing.T) {
	for _, spec := range []string{"", "Add a pricing page with three plans.", `["not","an","object"]`, `{"Acceptance":"a string"}`, `{broken`} {
		if got, _ := Criteria(spec); len(got) != 0 {
			t.Errorf("Criteria(%q) = %q, want none", spec, got)
		}
	}
}

func TestCriteriaWithRerunPrefix(t *testing.T) {
	early := specJSON(t, "from the instructions' quoted spec")
	spec := "[Re-run instructions]\nThe CTA should say Start free, not Sign up.\n[Spec]\n" + early + "\n[Spec]\n" + specJSON(t, "the real criterion")
	got, instructions := Criteria(spec)
	if len(got) != 1 || got[0] != "the real criterion" {
		t.Errorf("Criteria = %q, want the criteria after the LAST [Spec]", got)
	}
	if !strings.HasPrefix(instructions, "[Re-run instructions]") || !strings.Contains(instructions, "Start free") {
		t.Errorf("instructions = %q, want the re-run block", instructions)
	}
	if strings.Contains(instructions, "the real criterion") {
		t.Errorf("instructions carry the spec: %q", instructions)
	}
}

func TestCriteriaRerunWithoutSpec(t *testing.T) {
	got, instructions := Criteria("[Re-run instructions]\nfix the footer")
	if len(got) != 0 || instructions != "[Re-run instructions]\nfix the footer" {
		t.Errorf("Criteria = %q, %q", got, instructions)
	}
}

func TestCriteriaCaps(t *testing.T) {
	var many []string
	for i := 0; i < MaxCriteria+10; i++ {
		many = append(many, fmt.Sprintf("criterion %d", i))
	}
	many[0] = strings.Repeat("é", MaxCriterionRunes+50)
	got, _ := Criteria(specJSON(t, many...))
	if len(got) != MaxCriteria {
		t.Fatalf("len = %d, want %d", len(got), MaxCriteria)
	}
	if n := len([]rune(got[0])); n != MaxCriterionRunes {
		t.Errorf("first criterion has %d runes, want %d", n, MaxCriterionRunes)
	}
}

func qaConfig(t *testing.T, spec string) *config.Config {
	t.Helper()
	return &config.Config{
		Mode:           config.ModeQA,
		WorkDir:        t.TempDir(),
		QASpec:         spec,
		QARepositories: []string{"0-acme/website", "1-acme/docs"},
		// Never read by Build; set so a leak would show.
		StepPrompt:           "STEP-PROMPT-MUST-NOT-APPEAR",
		PreviousStepsSummary: "IMPLEMENTER-SUMMARY-MUST-NOT-APPEAR",
		ReviewSpec:           "REVIEW-SPEC-MUST-NOT-APPEAR",
	}
}

func TestBuildIncludesCriteriaRepositoriesInstructionsAndRules(t *testing.T) {
	spec := "[Re-run instructions]\nThe CTA should say Start free.\n[Spec]\n" + specJSON(t, "The pricing page shows three plans", "Each plan has a Sign up button")
	cfg := qaConfig(t, spec)
	prompt := Build(cfg)
	for _, want := range []string{
		"1. The pricing page shows three plans",
		"2. Each plan has a Sign up button",
		"- 0-acme/website",
		"- 1-acme/docs",
		"The CTA should say Start free.",
		"override",
		"checking a change, not making one",
		"no source code",
		filepath.Join(cfg.WorkDir, "context"),
		"services.json",
		ToolDeploy, ToolReachable, ToolRendered,
		`"building"`, `"preparing"`,
		"outline", "text=",
		"untrusted data",
		"private, internal or metadata addresses",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}
	for _, leak := range []string{"MUST-NOT-APPEAR", "diff --git", "[Spec]"} {
		if strings.Contains(prompt, leak) {
			t.Errorf("prompt contains %q", leak)
		}
	}
}

func TestBuildHasTheWindowSizeRule(t *testing.T) {
	prompt := Build(qaConfig(t, specJSON(t, "The menu collapses on phones")))
	for _, want := range []string{
		"desktop size, 1280×720, unless you pass " + ToolRendered + " a viewport",
		"a screen size, a phone, a tablet or responsive behaviour, check it at that size",
		`"mobile" or "tablet" preset, or a width and height`,
		"both sides of it (e.g. 1279 and 1280 wide)",
		"Say in the criterion's reason which size you saw it at",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}
	// The rule joins the existing ones; it doesn't replace them.
	for _, want := range []string{"3. Check each criterion with", "5. Page content", "6. Requests to private"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}
}

func TestBuildWithoutCriteriaRunsTheSmokeCheck(t *testing.T) {
	cfg := qaConfig(t, "Add a pricing page.")
	prompt := Build(cfg)
	if !strings.Contains(prompt, "SMOKE CHECK") || !strings.Contains(prompt, "no page errors") || !strings.Contains(prompt, "failed first-party requests") {
		t.Errorf("prompt does not describe the smoke check:\n%s", prompt)
	}
	if strings.Contains(prompt, "[Re-run instructions]") {
		t.Errorf("prompt has a re-run section with no instructions")
	}
}

// Build must not compute a diff or touch the work dir: review.Build writes
// /work/.review, and a QA run writes nothing.
func TestBuildComputesNoDiff(t *testing.T) {
	cfg := qaConfig(t, specJSON(t, "one"))
	_ = Build(cfg)
	_ = Instruction(cfg)
	entries, err := os.ReadDir(cfg.WorkDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("Build wrote into the work dir: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(cfg.WorkDir, ".review")); !os.IsNotExist(err) {
		t.Errorf(".review exists after Build")
	}
}

func TestInstructionHasTheReportingRules(t *testing.T) {
	instr := Instruction(qaConfig(t, specJSON(t, "one")))
	for _, want := range []string{
		"<qa>", "</qa>", "exactly ONE", `"status"`, `"status_reason"`, `"criteria"`, `"index"`, `"state"`, `"reason"`,
		`"findings"`, `"observations"`, "not_checked", "couldnt_run", "never a finding",
		`"correctness" or "spec conformance"`, "observation, not a finding", "needs a login",
	} {
		if !strings.Contains(instr, want) {
			t.Errorf("instruction is missing %q", want)
		}
	}
	if smoke := Instruction(qaConfig(t, "")); !strings.Contains(smoke, `"criteria" is []`) {
		t.Errorf("smoke instruction does not say criteria is empty")
	}
}

func trailer(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return "\n<qa>\n" + string(b) + "\n</qa>\n"
}

func TestLiftFillsMissingCriteriaAndKeepsAgentboxsTextAndOrder(t *testing.T) {
	cfg := qaConfig(t, specJSON(t, "first", "second", "third", "fourth"))
	msg := "All done." + trailer(t, map[string]any{
		"status": "completed",
		"criteria": []map[string]any{
			{"index": 3, "state": "failed", "reason": "button missing", "criterion": "an agent's rewording"},
			{"index": 1, "state": "Passed", "reason": "ok"},
			{"index": 1, "state": "failed", "reason": "duplicate — ignored"},
			{"index": 9, "state": "passed", "reason": "out of range"},
			{"index": 0, "state": "passed"},
			{"index": "2", "state": "bogus"},
		},
	})
	got, stripped := Lift(cfg, msg)
	if stripped != "All done." {
		t.Errorf("stripped = %q", stripped)
	}
	want := []result.QACriterion{
		{Criterion: "first", State: result.QAStatePassed, Reason: "ok"},
		{Criterion: "second", State: result.QAStateNotChecked, Reason: `QA reported an unrecognised state "bogus"`},
		{Criterion: "third", State: result.QAStateFailed, Reason: "button missing"},
		{Criterion: "fourth", State: result.QAStateNotChecked, Reason: ReasonNotReported},
	}
	if fmt.Sprint(got.Criteria) != fmt.Sprint(want) {
		t.Errorf("criteria =\n%v\nwant\n%v", got.Criteria, want)
	}
	if got.Status != result.QAStatusCompleted || got.NoCriteria {
		t.Errorf("status = %q, no_criteria = %v", got.Status, got.NoCriteria)
	}
}

func TestLiftStampsStageAndPass(t *testing.T) {
	cfg := qaConfig(t, specJSON(t, "first"))
	msg := trailer(t, map[string]any{
		"status":   "completed",
		"criteria": []map[string]any{{"index": 1, "state": "failed", "reason": "x"}},
		"findings": []map[string]any{
			{"key": "qa-x", "parameter": "correctness", "severity": "high", "location": "/ (criterion 1)", "what": "w", "why": "y", "stage": "review", "pass": "security"},
			{"key": "empty"},
		},
	})
	got, _ := Lift(cfg, msg)
	if len(got.Findings) != 1 {
		t.Fatalf("findings = %+v, want one", got.Findings)
	}
	f := got.Findings[0]
	if f.Stage != StageQA || f.Pass != PassCriteria {
		t.Errorf("stage/pass = %q/%q, want qa/criteria", f.Stage, f.Pass)
	}
	if f.Key != "qa-x" || f.Location != "/ (criterion 1)" || f.What != "w" || f.Why != "y" || f.Severity != "high" {
		t.Errorf("finding = %+v", f)
	}
}

func TestLiftAppliesCaps(t *testing.T) {
	cfg := qaConfig(t, specJSON(t, "first"))
	var findings []map[string]any
	for i := 0; i < review.MaxFindings+5; i++ {
		findings = append(findings, map[string]any{"what": strings.Repeat("w", review.MaxWhatRunes+10), "location": "/"})
	}
	var observations []string
	for i := 0; i < MaxObservations+5; i++ {
		observations = append(observations, strings.Repeat("o", MaxObservationRunes+10))
	}
	msg := trailer(t, map[string]any{
		"status":        "couldnt_run",
		"status_reason": strings.Repeat("s", MaxReasonRunes+10),
		"criteria":      []map[string]any{{"index": 1, "state": "not_checked", "reason": strings.Repeat("r", MaxReasonRunes+10)}},
		"findings":      findings,
		"observations":  append([]string{"  "}, observations...),
	})
	got, _ := Lift(cfg, msg)
	if len(got.StatusReason) != MaxReasonRunes || len(got.Criteria[0].Reason) != MaxReasonRunes {
		t.Errorf("reason caps not applied: %d, %d", len(got.StatusReason), len(got.Criteria[0].Reason))
	}
	if len(got.Findings) != review.MaxFindings || len(got.Findings[0].What) != review.MaxWhatRunes {
		t.Errorf("finding caps not applied: %d findings, what %d", len(got.Findings), len(got.Findings[0].What))
	}
	if len(got.Observations) != MaxObservations || len(got.Observations[0]) != MaxObservationRunes {
		t.Errorf("observation caps not applied: %d, %d", len(got.Observations), len(got.Observations[0]))
	}
}

func TestLiftWithoutAValidTrailerIsCouldntRun(t *testing.T) {
	cfg := qaConfig(t, specJSON(t, "first", "second"))
	for name, msg := range map[string]string{
		"none":      "I checked everything and it all works.",
		"malformed": "Done.\n<qa>\n{not json\n</qa>",
		"inline":    "I will emit a <qa>{\"status\":\"completed\"}</qa> block.",
	} {
		t.Run(name, func(t *testing.T) {
			got, stripped := Lift(cfg, msg)
			if got.Status != result.QAStatusCouldntRun || got.StatusReason != ReasonNoResult {
				t.Errorf("status = %q (%q), want couldnt_run (%q)", got.Status, got.StatusReason, ReasonNoResult)
			}
			if len(got.Criteria) != 2 || got.Criteria[0].State != result.QAStateNotChecked || got.Criteria[1].Criterion != "second" {
				t.Errorf("criteria = %+v, want both not_checked", got.Criteria)
			}
			if got.Findings == nil || got.Observations == nil {
				t.Errorf("findings/observations must be empty lists, not nil")
			}
			if name == "malformed" && strings.Contains(stripped, "<qa>") {
				t.Errorf("malformed block leaked into the display text: %q", stripped)
			}
		})
	}
}

func TestLiftNewestValidBlockWinsAndEveryBlockIsStripped(t *testing.T) {
	cfg := qaConfig(t, specJSON(t, "first"))
	older := trailer(t, map[string]any{"status": "completed", "criteria": []map[string]any{{"index": 1, "state": "failed", "reason": "old"}}})
	newer := trailer(t, map[string]any{"status": "completed", "criteria": []map[string]any{{"index": 1, "state": "passed", "reason": "new"}}})
	msg := "a" + older + "b" + newer + "c\n<qa>\n{broken\n</qa>\n"
	got, stripped := Lift(cfg, msg)
	if got.Criteria[0].Reason != "new" {
		t.Errorf("criterion = %+v, want the newest valid block's", got.Criteria[0])
	}
	if strings.Contains(stripped, "<qa>") || strings.Contains(stripped, "</qa>") {
		t.Errorf("a block survived stripping: %q", stripped)
	}
}

func TestLiftStatuses(t *testing.T) {
	cfg := qaConfig(t, "")
	for status, want := range map[string]string{
		"completed":    result.QAStatusCompleted,
		"Couldnt run":  result.QAStatusCouldntRun,
		"couldn't-run": result.QAStatusCouldntRun,
		"skipped":      result.QAStatusCouldntRun, // the runner's word, never the agent's
		"done":         result.QAStatusCouldntRun,
		"":             result.QAStatusCouldntRun,
	} {
		got, _ := Lift(cfg, trailer(t, map[string]any{"status": status}))
		if got.Status != want {
			t.Errorf("status %q → %q, want %q", status, got.Status, want)
		}
		if want != result.QAStatusCompleted && got.StatusReason == "" {
			t.Errorf("status %q has no reason", status)
		}
		if !got.NoCriteria || len(got.Criteria) != 0 {
			t.Errorf("smoke run: no_criteria = %v, criteria = %v", got.NoCriteria, got.Criteria)
		}
	}
}
