package agent

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/config"
	"github.com/deployment-io/agentbox/internal/result"
)

const qaTrailer = "Checked the pricing page.\n\n<qa>\n" +
	`{"status":"completed","criteria":[{"index":1,"state":"failed","reason":"no Sign up button"}],` +
	`"findings":[{"key":"qa-signup","parameter":"spec conformance","severity":"high","location":"/pricing (criterion 1)","what":"no Sign up button","why":"visitors cannot sign up"}]}` +
	"\n</qa>\n"

// qaDriver records what Run handed it and reports a final message carrying
// a <qa> trailer.
type qaDriver struct {
	promptAtBuild string
	final         string
	turns         int
}

func (d *qaDriver) Ensure(context.Context) error { return nil }
func (d *qaDriver) Binary() string               { return "true" }
func (d *qaDriver) BuildArgs(cfg *config.Config) []string {
	d.promptAtBuild = cfg.StepPrompt
	return nil
}
func (d *qaDriver) Stdin(*config.Config) string   { return "" }
func (d *qaDriver) DetectVersion() string         { return "test" }
func (d *qaDriver) Capabilities() Capabilities    { return Capabilities{MCPTools: true} }
func (d *qaDriver) AllowedHosts() []string        { return nil }
func (d *qaDriver) NewOutputParser() OutputParser { return &fixedParser{d.final, d.turns} }
func (d *qaDriver) NewLogFormatter(sink io.Writer) io.WriteCloser {
	return passthroughWriteCloser{sink}
}

type fixedParser struct {
	final string
	turns int
}

func (*fixedParser) Consume(r io.Reader) { _, _ = io.Copy(io.Discard, r) }
func (p *fixedParser) State() ParsedState {
	return ParsedState{
		ChangesSummary: p.final,
		Turns:          p.turns,
		FilesChanged:   []string{"stray"},
		PRTitle:        "stray title",
		TokenUsage:     result.TokenUsage{InputTokens: 10, OutputTokens: 5},
	}
}

func TestRunInQAModeLiftsTheTrailer(t *testing.T) {
	workDir := t.TempDir()
	writeFile(t, filepath.Join(workDir, "context", "services.json"), "{}\n")
	t.Setenv("RESULT_PATH", filepath.Join(t.TempDir(), "result.json"))

	driver := &qaDriver{final: qaTrailer, turns: 7}
	cfg := &config.Config{
		Mode:           config.ModeQA,
		WorkDir:        workDir,
		AgentType:      "claude-code",
		QASpec:         `{"Acceptance":["the pricing page has a Sign up button","the footer links to /about"]}`,
		QARepositories: []string{"0-acme/website"},
		MCPSocket:      "/run/agentbox/tool-rpc.sock",
	}
	oc := Run(context.Background(), cfg, driver)

	if !strings.Contains(driver.promptAtBuild, "1. the pricing page has a Sign up button") || !strings.Contains(driver.promptAtBuild, "0-acme/website") {
		t.Errorf("the driver was not handed the QA prompt: %q", driver.promptAtBuild)
	}
	if strings.Contains(driver.promptAtBuild, "Repositories for this task are checked out at") {
		t.Error("the QA prompt was anchored to repositories")
	}
	if oc.Status != result.StatusSuccess {
		t.Fatalf("status = %q (%s)", oc.Status, oc.Error)
	}
	if strings.Contains(oc.ChangesSummary, "<qa>") || oc.ChangesSummary != "Checked the pricing page." {
		t.Errorf("changes_summary = %q", oc.ChangesSummary)
	}
	qr := oc.QAResult
	if qr == nil {
		t.Fatal("no qa_result")
	}
	if qr.Status != result.QAStatusCompleted || len(qr.Criteria) != 2 || qr.Criteria[0].State != result.QAStateFailed || qr.Criteria[1].State != result.QAStateNotChecked {
		t.Errorf("qa_result = %+v", qr)
	}
	if len(qr.Findings) != 1 || qr.Findings[0].Stage != "qa" || qr.Findings[0].Pass != "criteria" {
		t.Errorf("findings = %+v", qr.Findings)
	}
	if oc.FilesChanged != nil || oc.VerifyResult != nil || oc.PRTitle != "" {
		t.Error("the implementer's fields survived a QA run")
	}
	if oc.Turns != 7 || oc.TokenUsage.InputTokens != 10 {
		t.Errorf("usage not reported: turns %d, tokens %+v", oc.Turns, oc.TokenUsage)
	}
	if oc.ReviewResult != nil || oc.ReviewEffort != nil {
		t.Error("a QA run carries review fields")
	}
	if _, err := os.Stat(filepath.Join(workDir, ".review")); !os.IsNotExist(err) {
		t.Error("a QA run wrote a .review directory")
	}
}

// A run that did not finish cannot claim the criteria were checked.
func TestLiftQAOnAnUnfinishedRunIsCouldntRun(t *testing.T) {
	cfg := &config.Config{Mode: config.ModeQA, QASpec: `{"Acceptance":["one"]}`}
	oc := result.Outcome{Status: result.StatusTimeout, ChangesSummary: qaTrailer, Error: "no agent output for 10m0s"}
	liftQA(&oc, cfg)
	if oc.QAResult.Status != result.QAStatusCouldntRun || !strings.Contains(oc.QAResult.StatusReason, "timeout") {
		t.Errorf("qa_result = %+v", oc.QAResult)
	}

	none := result.Outcome{Status: result.StatusSuccess, ChangesSummary: "all good"}
	liftQA(&none, cfg)
	if none.QAResult.Status != result.QAStatusCouldntRun || none.QAResult.StatusReason != "QA didn't report a result" {
		t.Errorf("no trailer: qa_result = %+v", none.QAResult)
	}
}

// An error whose final message opens with the trailer must still have the
// whole block stripped: the failure prefix must not join the opening tag's
// line, or qa.Strip's own-line matcher misses it and the JSON leaks.
func TestFailureMessageKeepsALeadingTrailerStrippable(t *testing.T) {
	state := ParsedState{IsError: true, ChangesSummary: strings.TrimPrefix(qaTrailer, "Checked the pricing page.\n\n")}
	oc := result.Outcome{Status: result.StatusFailure, Error: failureMessage(nil, state, "", "claude")}
	liftQA(&oc, &config.Config{Mode: config.ModeQA})
	if strings.Contains(oc.Error, "<qa>") || strings.Contains(oc.Error, "qa-signup") {
		t.Fatalf("trailer leaked into Error: %q", oc.Error)
	}
	if strings.Contains(oc.QAResult.StatusReason, "qa-signup") {
		t.Fatalf("trailer leaked into status_reason: %q", oc.QAResult.StatusReason)
	}
}
