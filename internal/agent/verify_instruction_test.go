package agent_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/agent"
	"github.com/deployment-io/agentbox/internal/claude"
	"github.com/deployment-io/agentbox/internal/codex"
	"github.com/deployment-io/agentbox/internal/config"
	"github.com/deployment-io/agentbox/internal/opencode"
)

// The <verify> trailer is one contract read by one parser-and-runner pipeline,
// but each driver carries its own copy of the words that ask for it (see the
// Rule-of-Three note in the opencode driver). The copies drifted: claude asked
// for stderr_tail and showed a single-repository failure, codex and opencode
// showed only the multi-repo example — so a one-repo failure on those two
// arrived as "agent self-verification failed: go test ./..." and nothing about
// why, which is the exact hole stderr_tail was added to close.
//
// This compares the EXTRACTED paragraph rather than a keyword, so a future edit
// to one driver's copy fails here instead of quietly reopening the gap. Only
// the paragraph: each agent keeps its own wording elsewhere ("final assistant
// message" for claude, "final message" for codex and opencode).
//
// It lives in internal/agent because this is the package all three drivers
// import, so it is the only place that can see all three at once without a
// cycle — and they are reached through their public BuildArgs, which is what
// the agent actually receives.

const stepPrompt = "do the thing"

func implementInstruction(t *testing.T, name string) string {
	t.Helper()
	cfg := &config.Config{Mode: config.ModeBatch, StepPrompt: stepPrompt}
	switch name {
	case "claude-code":
		args := claude.NewDriver("").BuildArgs(cfg)
		for i, a := range args {
			if a == "--append-system-prompt" && i+1 < len(args) {
				return args[i+1]
			}
		}
		t.Fatalf("claude passed no --append-system-prompt: %v", args)
	case "codex":
		return foldedInstruction(t, name, codex.NewDriver("").BuildArgs(cfg))
	case "opencode":
		return foldedInstruction(t, name, opencode.NewDriver("").BuildArgs(cfg))
	}
	t.Fatalf("unknown agent %q", name)
	return ""
}

// codex and opencode have no system-prompt flag, so the instruction rides on
// the prompt as the final argument.
func foldedInstruction(t *testing.T, name string, args []string) string {
	t.Helper()
	if len(args) == 0 {
		t.Fatalf("%s built no arguments", name)
	}
	last := args[len(args)-1]
	instruction, ok := strings.CutPrefix(last, stepPrompt+"\n\n")
	if !ok {
		t.Fatalf("%s did not fold the instruction onto the prompt: %q", name, last)
	}
	return instruction
}

// verifyParagraph is item 2 of the final-message format: the <verify> shape,
// its examples and the stakes, up to where item 3 (the PR title) begins.
func verifyParagraph(t *testing.T, name, instruction string) string {
	t.Helper()
	start := strings.Index(instruction, "2. The verification result")
	if start < 0 {
		t.Fatalf("%s's instruction has no <verify> paragraph:\n%s", name, instruction)
	}
	rest := instruction[start:]
	end := strings.Index(rest, "\n\n3. ")
	if end < 0 {
		t.Fatalf("%s's <verify> paragraph is not followed by item 3:\n%s", name, rest)
	}
	return rest[:end]
}

func TestVerifyParagraphIsIdenticalAcrossAgents(t *testing.T) {
	want := verifyParagraph(t, "claude-code", implementInstruction(t, "claude-code"))

	// The parts the downstream pipeline depends on, so three identical copies
	// cannot drift together into a paragraph that asks for less.
	for _, required := range []string{
		`"stderr_tail"`,
		"verbatim",
		"When passed is false",
		`"steps"`,
		`"repo"`,
		"blocks the commit and push",
	} {
		if !strings.Contains(want, required) {
			t.Fatalf("the reference paragraph lost %q:\n%s", required, want)
		}
	}
	// Both failure examples: the single-repository one is what a one-repo Task
	// copies, and it is the one codex and opencode were missing.
	if strings.Count(want, `"passed":false`) < 2 {
		t.Fatalf("the paragraph must show a failure for one repository AND for several:\n%s", want)
	}

	for _, name := range []string{"codex", "opencode"} {
		got := verifyParagraph(t, name, implementInstruction(t, name))
		if got != want {
			t.Errorf("%s's <verify> paragraph differs from claude-code's.\n\n%s:\n%s\n\nclaude-code:\n%s",
				name, name, got, want)
		}
	}
}

// Every agent that can be asked to implement is covered above. A fourth driver
// added without a fourth copy of the paragraph would otherwise ship with a
// verify contract nobody compared.
//
// The registry alone cannot say that: it lists only the drivers linked into
// THIS test binary, so a new driver package nothing here imports would leave it
// at three and the loop would pass having compared nothing new. So the source
// tree is counted too — every non-test file under internal/ that registers a
// driver — and the two must agree before the loop means anything.
func TestEveryImplementAgentIsCoveredByTheVerifyComparison(t *testing.T) {
	compared := map[string]bool{"claude-code": true, "codex": true, "opencode": true}
	registered := agent.RegisteredTypes()
	if inSource := driverRegistrationsInSource(t); len(registered) != inSource {
		t.Fatalf("%d driver(s) register themselves in internal/, but %d are linked into this test %v: "+
			"import the new driver here and compare its <verify> paragraph", inSource, len(registered), registered)
	}
	for _, name := range registered {
		if !compared[name] {
			t.Errorf("agent %q is registered but its <verify> paragraph is compared with nobody's", name)
		}
	}
}

// driverRegistrationsInSource counts the non-test Go files under internal/
// that call agent.Register — one per driver package.
func driverRegistrationsInSource(t *testing.T) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(src), "agent.Register(") {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking internal/ for driver registrations: %v", err)
	}
	if count == 0 {
		t.Fatal("found no driver registrations under internal/ — the walk is looking in the wrong place")
	}
	return count
}
