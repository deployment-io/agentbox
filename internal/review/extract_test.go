package review

import (
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/config"
)

const validBlock = `<review>
{"findings":[{"key":"sec-1","parameter":"security","severity":"high","location":"0-acme/api/handler.go:41","what":"no auth check","why":"any caller can read another org's data","pass":"security"}],"coverage":[{"parameter":"security","state":"checked"}]}
</review>`

func TestExtractLiftsFindingsAndStripsTheBlock(t *testing.T) {
	text := "I reviewed the change and found one thing.\n\n" + validBlock

	findings, coverage, _, stripped, ok := Extract(text, nil)
	if !ok {
		t.Fatal("Extract reported not-ok for a well-formed block")
	}
	if len(findings) != 1 || findings[0].Key != "sec-1" || findings[0].Location != "0-acme/api/handler.go:41" {
		t.Errorf("findings = %+v, want the one security finding", findings)
	}
	if len(coverage) != 1 || coverage[0].Parameter != "security" {
		t.Errorf("coverage = %+v, want the one claimed entry", coverage)
	}
	if strings.Contains(stripped, "<review>") || strings.Contains(stripped, "findings") {
		t.Errorf("stripped text still carries the block: %q — it would be rendered into the PR body verbatim", stripped)
	}
	if stripped != "I reviewed the change and found one thing." {
		t.Errorf("stripped = %q, want the prose alone", stripped)
	}
}

// The stage is agentbox's to assign. An agent that labels its finding as
// coming from some other stage must not be believed, or a finding could
// present itself as evidence from a stage that never ran.
func TestExtractStampsTheStageRegardlessOfWhatTheAgentSaid(t *testing.T) {
	text := `<review>
{"findings":[{"parameter":"security","severity":"high","what":"x","location":"a.go:1","stage":"implement"}]}
</review>`

	findings, _, _, _, ok := Extract(text, nil)
	if !ok || len(findings) != 1 {
		t.Fatalf("Extract = %+v, ok=%t", findings, ok)
	}
	if findings[0].Stage != StageReview {
		t.Errorf("stage = %q, want %q — the agent's value must be discarded", findings[0].Stage, StageReview)
	}
}

// A prose mention of the tag at the start of a line must not open a block and
// swallow the real one — the failure internal/spec hit in production.
func TestExtractIgnoresAProseMentionAndTakesTheLatestBlock(t *testing.T) {
	text := "I will end with a <review> block as instructed.\n\n" +
		`<review>
{"findings":[{"key":"old","parameter":"security","severity":"low","what":"earlier","location":"a.go:1"}]}
</review>` + "\n\nOn reflection, here is the final answer.\n\n" +
		`<review>
{"findings":[{"key":"new","parameter":"correctness","severity":"high","what":"later","location":"b.go:2"}]}
</review>`

	findings, _, _, stripped, ok := Extract(text, nil)
	if !ok {
		t.Fatal("Extract reported not-ok")
	}
	if len(findings) != 1 || findings[0].Key != "new" {
		t.Errorf("findings = %+v, want only the latest block's finding", findings)
	}
	if strings.Contains(stripped, "{\"findings\"") {
		t.Errorf("stripped text still carries a block: %q", stripped)
	}
	if !strings.Contains(stripped, "I will end with a") {
		t.Errorf("stripped text lost the prose: %q", stripped)
	}
}

// An unclosed opening must not claim the real block's closing tag, and must
// not mask the real block either.
func TestExtractSurvivesAnUnclosedOpening(t *testing.T) {
	text := "<review>\noops, never closed this one\n\n" + validBlock

	findings, _, _, _, ok := Extract(text, nil)
	if !ok || len(findings) != 1 || findings[0].Key != "sec-1" {
		t.Errorf("Extract = (%+v, ok=%t), want the well-formed block", findings, ok)
	}
}

// A malformed block is "no review reported", never an error — but it must
// still be stripped, because the alternative is raw JSON in a PR body.
func TestExtractStripsAMalformedBlock(t *testing.T) {
	text := "Here is what I found.\n\n<review>\nnot json at all\n</review>"

	findings, _, _, stripped, ok := Extract(text, nil)
	if ok || len(findings) != 0 {
		t.Errorf("Extract = (%+v, ok=%t), want not-ok with no findings", findings, ok)
	}
	if strings.Contains(stripped, "not json at all") || strings.Contains(stripped, "<review>") {
		t.Errorf("stripped = %q, want the malformed block removed anyway", stripped)
	}
}

// Caps truncate rather than reject: a review that found 400 things is still
// worth its first hundred.
func TestExtractAppliesTheCaps(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<review>` + "\n" + `{"findings":[`)
	for i := 0; i < MaxFindings+20; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"parameter":"security","severity":"low","location":"` + strings.Repeat("l", MaxLocationRunes+50) +
			`","what":"` + strings.Repeat("w", MaxWhatRunes+50) +
			`","why":"` + strings.Repeat("y", MaxWhyRunes+50) +
			`","key":"` + strings.Repeat("k", MaxKeyRunes+50) +
			`","pass":"` + strings.Repeat("p", MaxPassRunes+50) + `"}`)
	}
	b.WriteString(`],"coverage":[{"parameter":"security","state":"checked","reason":"` + strings.Repeat("r", MaxReasonRunes+50) + `"}]}` + "\n</review>")

	findings, coverage, _, _, ok := Extract(b.String(), nil)
	if !ok {
		t.Fatal("Extract reported not-ok")
	}
	if len(findings) != MaxFindings {
		t.Errorf("findings = %d, want %d", len(findings), MaxFindings)
	}
	f := findings[0]
	for _, c := range []struct {
		name  string
		got   string
		limit int
	}{
		{"key", f.Key, MaxKeyRunes},
		{"location", f.Location, MaxLocationRunes},
		{"what", f.What, MaxWhatRunes},
		{"why", f.Why, MaxWhyRunes},
		{"pass", f.Pass, MaxPassRunes},
		{"reason", coverage[0].Reason, MaxReasonRunes},
	} {
		if len([]rune(c.got)) != c.limit {
			t.Errorf("%s length = %d, want %d", c.name, len([]rune(c.got)), c.limit)
		}
	}
}

// Truncation is by rune: cutting bytes would leave invalid UTF-8 in a field
// headed for a PR body.
func TestExtractTruncatesOnRuneBoundaries(t *testing.T) {
	text := `<review>
{"findings":[{"parameter":"security","severity":"low","location":"a.go:1","what":"` + strings.Repeat("é", MaxWhatRunes+20) + `"}]}
</review>`

	findings, _, _, _, ok := Extract(text, nil)
	if !ok || len(findings) != 1 {
		t.Fatalf("Extract = (%+v, ok=%t)", findings, ok)
	}
	if findings[0].What != strings.Repeat("é", MaxWhatRunes) {
		t.Errorf("what was not cut on a rune boundary (len %d runes)", len([]rune(findings[0].What)))
	}
}

// A finding with nothing to act on is not a finding. Carrying it would pad the
// PR body with empty rows and, if it named a gated parameter, could hold up a
// pull request over nothing.
func TestExtractDropsAFindingWithNoWhatAndNoLocation(t *testing.T) {
	text := `<review>
{"findings":[{"parameter":"security","severity":"critical","why":"trust me"},{"parameter":"security","severity":"low","what":"real one","location":"a.go:1"}]}
</review>`

	findings, _, _, _, _ := Extract(text, nil)
	if len(findings) != 1 || findings[0].What != "real one" {
		t.Errorf("findings = %+v, want only the actionable one", findings)
	}
}

// The status list is bounded by the findings that were GIVEN: a status for a
// key nobody asked about says nothing about the change, and an unreadable
// status word would have to be guessed at — with "resolved" the guess that
// clears a must-fix nobody fixed.
func TestExtractParsesPreviousStatuses(t *testing.T) {
	open := []config.ReviewOpenFinding{
		{Key: "sec-env-dump", Parameter: "security", Severity: "critical"},
		{Key: "cor-missing-nil-check", Parameter: "correctness", Severity: "high"},
		{Key: "sec-weak-hash", Parameter: "security", Severity: "medium"},
	}
	text := `<review>
{"findings":[],"previous":[
{"key":"sec-env-dump","status":"Still_Present","note":"  the route still returns process.env; only a comment was added  "},
{"key":"cor-missing-nil-check","status":" RESOLVED ","note":"the handler now checks for nil"},
{"key":"sec-weak-hash","status":"mitigated","note":"we documented it"},
{"key":"sec-invented-key","status":"resolved","note":"nobody asked about this"}
]}
</review>`

	_, _, previous, _, ok := Extract(text, open)
	if !ok {
		t.Fatal("Extract reported not-ok for a well-formed block")
	}
	if len(previous) != 2 {
		t.Fatalf("previous = %+v, want only the two answerable entries", previous)
	}
	if previous[0].Key != "sec-env-dump" || previous[0].Status != StatusStillPresent {
		t.Errorf("previous[0] = %+v, want sec-env-dump still_present with its case normalised", previous[0])
	}
	if !strings.HasPrefix(previous[0].Note, "the route still returns") {
		t.Errorf("note = %q, want it trimmed", previous[0].Note)
	}
	if previous[1].Key != "cor-missing-nil-check" || previous[1].Status != StatusResolved {
		t.Errorf("previous[1] = %+v, want cor-missing-nil-check resolved", previous[1])
	}
	// sec-weak-hash carried a status this contract does not define and
	// sec-invented-key was never asked about. Both are simply absent, and
	// absence is read by the consumer as still present.
	for _, p := range previous {
		if p.Key == "sec-weak-hash" || p.Key == "sec-invented-key" {
			t.Errorf("previous carries %+v, which should have been dropped", p)
		}
	}
}

// A round that was given nothing reports no status list at all, whatever the
// agent emitted — there is nothing for an entry to refer to.
func TestExtractDropsPreviousWhenNothingWasGiven(t *testing.T) {
	text := `<review>
{"findings":[],"previous":[{"key":"sec-env-dump","status":"resolved","note":"trust me"}]}
</review>`

	_, _, previous, _, ok := Extract(text, nil)
	if !ok {
		t.Fatal("Extract reported not-ok")
	}
	if len(previous) != 0 {
		t.Errorf("previous = %+v, want none — no findings were given", previous)
	}
}

// The list is capped at the number of findings given (one answer per
// question), and the note is capped like every other free-text field.
func TestExtractCapsPreviousListAndNote(t *testing.T) {
	open := []config.ReviewOpenFinding{{Key: "sec-env-dump"}, {Key: "cor-nil"}}
	text := `<review>
{"findings":[],"previous":[
{"key":"sec-env-dump","status":"still_present","note":"` + strings.Repeat("n", MaxNoteRunes+50) + `"},
{"key":"sec-env-dump","status":"resolved","note":"second answer to the same question"},
{"key":"cor-nil","status":"resolved","note":"fixed"}
]}
</review>`

	_, _, previous, _, _ := Extract(text, open)
	if len(previous) != 2 {
		t.Fatalf("previous = %+v, want one entry per key", previous)
	}
	if got := len([]rune(previous[0].Note)); got != MaxNoteRunes {
		t.Errorf("note length = %d runes, want %d", got, MaxNoteRunes)
	}
	if previous[0].Status != StatusStillPresent {
		t.Errorf("previous[0] = %+v — the first answer for a key stands; a later one must not overwrite it", previous[0])
	}
}

// LiftResult carries the status list through to result.json, and an
// unreadable trailer clears nothing: no entry means still present.
func TestLiftResultCarriesPreviousThrough(t *testing.T) {
	open := []config.ReviewOpenFinding{{Key: "sec-env-dump", Severity: "critical"}}
	planned := BuildCoverage([]string{PassSecurity, PassCorrectness}, nil)
	text := `<review>
{"findings":[],"coverage":[],"previous":[{"key":"sec-env-dump","status":"still_present","note":"the route is unchanged"}]}
</review>`

	result, _ := LiftResult(text, planned, open)
	if len(result.Previous) != 1 || result.Previous[0].Status != StatusStillPresent {
		t.Fatalf("previous = %+v, want the one still-present status", result.Previous)
	}

	missing, _ := LiftResult("I forgot the block.", planned, open)
	if len(missing.Previous) != 0 {
		t.Errorf("previous = %+v, want none — a missing trailer resolves nothing", missing.Previous)
	}
}

// LiftResult ships agentbox's coverage, not the agent's — except where the
// agent admits a gap agentbox could not see.
func TestLiftResultKeepsAgentboxCoverageAndHonoursAnAdmittedGap(t *testing.T) {
	planned := BuildCoverage([]string{PassSecurity, PassCorrectness}, nil)
	text := `<review>
{"findings":[],"coverage":[{"parameter":"security","state":"checked"},{"parameter":"correctness","state":"skipped","reason":"the diff was too large to read fully"},{"parameter":"performance","state":"checked"}]}
</review>`

	result, _ := LiftResult(text, planned, nil)
	byParameter := map[string]Coverage{}
	for _, c := range result.Coverage {
		byParameter[c.Parameter] = c
	}
	if len(result.Coverage) != len(allParameters) {
		t.Errorf("coverage has %d entries, want one per parameter (%d)", len(result.Coverage), len(allParameters))
	}
	if got := byParameter["correctness"]; got.State != stateSkipped || !strings.Contains(got.Reason, "too large") {
		t.Errorf("correctness coverage = %+v, want the agent's admitted gap", got)
	}
	// The agent claimed to have checked performance. No pass ran for it, and
	// believing that claim would report coverage nobody produced.
	if got := byParameter["performance"]; got.State != stateNotChecked {
		t.Errorf("performance coverage = %+v, want not checked — no pass ran for it", got)
	}
}

// A review whose trailer never arrived still has to report what it examined:
// the run happened, and "nothing reported" is not the same as "nothing ran".
func TestLiftResultKeepsCoverageWhenTheTrailerIsMissing(t *testing.T) {
	planned := BuildCoverage([]string{PassSecurity}, map[string]string{PassCorrectness: ReasonLockfileOnly})

	result, stripped := LiftResult("I looked at the diff but forgot the block.", planned, nil)
	if len(result.Coverage) != len(allParameters) {
		t.Errorf("coverage has %d entries, want one per parameter", len(result.Coverage))
	}
	if len(result.Findings) != 0 {
		t.Errorf("findings = %+v, want none", result.Findings)
	}
	if stripped != "I looked at the diff but forgot the block." {
		t.Errorf("stripped = %q, want the prose unchanged", stripped)
	}
}
