package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/config"
	"github.com/deployment-io/agentbox/internal/result"
	"github.com/deployment-io/agentbox/internal/review"
)

// envDumpDriver's "agent" is a shell that writes its own environment to a
// file, so a test can see exactly what an agent process inherits.
type envDumpDriver struct {
	recordingDriver
	outFile string
}

func (d *envDumpDriver) Binary() string { return "sh" }
func (d *envDumpDriver) BuildArgs(cfg *config.Config) []string {
	d.built = true
	return []string{"-c", "env > " + d.outFile}
}

// CLAUDE_CODE_EFFORT_LEVEL overrides claude's --effort. Effort is the
// runner's decision, so no agent process may inherit it — in any mode.
func TestNoAgentProcessInheritsClaudeCodeEffortLevel(t *testing.T) {
	t.Setenv("CLAUDE_CODE_EFFORT_LEVEL", "max")
	t.Setenv("REVIEW_EFFORT", "high")

	// buildEnv is the env of every agent process: Run (batch and review) and
	// RunInteractive both use it.
	for _, kv := range buildEnv() {
		if strings.HasPrefix(kv, "CLAUDE_CODE_EFFORT_LEVEL=") || strings.HasPrefix(kv, "REVIEW_EFFORT=") {
			t.Errorf("buildEnv forwards %q", kv)
		}
	}

	workDir := t.TempDir()
	repo := filepath.Join(workDir, "0-acme", "api")
	base := initTestRepo(t, repo)
	writeFile(t, filepath.Join(repo, "main.go"), "package main\n// changed\n")
	t.Setenv("RESULT_PATH", filepath.Join(t.TempDir(), "result.json"))

	for _, cfg := range []*config.Config{
		{Mode: config.ModeBatch, WorkDir: workDir, AgentType: "claude-code", StepPrompt: "do the thing"},
		{
			Mode: config.ModeReview, WorkDir: workDir, AgentType: "claude-code", ReviewEffort: "high",
			ReviewPasses: []string{review.PassSecurity}, ReviewBaseCommits: map[string]string{"0-acme/api": base}, ReviewRound: 1,
		},
	} {
		t.Run(cfg.Mode, func(t *testing.T) {
			driver := &envDumpDriver{outFile: filepath.Join(t.TempDir(), "env.txt")}
			if oc := Run(context.Background(), cfg, driver); oc.Status != result.StatusSuccess {
				t.Fatalf("status = %s (%s), want success", oc.Status, oc.Error)
			}
			env, err := os.ReadFile(driver.outFile)
			if err != nil {
				t.Fatalf("the agent never ran: %v", err)
			}
			for _, line := range strings.Split(string(env), "\n") {
				if strings.HasPrefix(line, "CLAUDE_CODE_EFFORT_LEVEL=") || strings.HasPrefix(line, "REVIEW_EFFORT=") {
					t.Errorf("the agent process inherited %q", line)
				}
			}
		})
	}
}

func TestLogReviewEffort(t *testing.T) {
	for effort, want := range map[string]string{
		"":     "[agentbox] review effort: model default\n",
		"high": "[agentbox] review effort: high\n",
	} {
		var buf bytes.Buffer
		logReviewEffort(&config.Config{ReviewEffort: effort}, &buf)
		if buf.String() != want {
			t.Errorf("effort %q logged %q, want %q", effort, buf.String(), want)
		}
	}
}

// result.json says which effort a review ran at — "" for the model's default
// — and says nothing about effort outside review mode.
func TestResultCarriesReviewEffortInReviewMode(t *testing.T) {
	workDir := t.TempDir()
	repo := filepath.Join(workDir, "0-acme", "api")
	base := initTestRepo(t, repo)
	writeFile(t, filepath.Join(repo, "main.go"), "package main\n// changed\n")

	run := func(t *testing.T, cfg *config.Config) map[string]any {
		t.Helper()
		path := filepath.Join(t.TempDir(), "result.json")
		t.Setenv("RESULT_PATH", path)
		driver := &stdinDriver{outFile: filepath.Join(t.TempDir(), "stdin.txt")}
		if err := result.Write(Run(context.Background(), cfg, driver)); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	reviewCfg := func(effort string, commits map[string]string) *config.Config {
		return &config.Config{
			Mode: config.ModeReview, WorkDir: workDir, AgentType: "claude-code", ReviewEffort: effort,
			ReviewPasses: []string{review.PassSecurity}, ReviewBaseCommits: commits, ReviewRound: 1,
		}
	}

	for _, tc := range []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{"set", reviewCfg("xhigh", map[string]string{"0-acme/api": base}), "xhigh"},
		{"model default", reviewCfg("", map[string]string{"0-acme/api": base}), ""},
		// The diff cannot be computed, so the agent never starts — the field
		// is still there.
		{"failed before the agent", reviewCfg("low", map[string]string{"0-acme/api": "deadbeef"}), "low"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := run(t, tc.cfg)
			v, ok := got["review_effort"]
			if !ok {
				t.Fatalf("result.json has no review_effort: %v", got)
			}
			if v != tc.want {
				t.Errorf("review_effort = %v, want %q", v, tc.want)
			}
		})
	}

	t.Run("batch", func(t *testing.T) {
		got := run(t, &config.Config{Mode: config.ModeBatch, WorkDir: workDir, AgentType: "claude-code", StepPrompt: "x", ReviewEffort: "high"})
		if v, ok := got["review_effort"]; ok {
			t.Errorf("batch result.json carries review_effort = %v", v)
		}
	})
}
