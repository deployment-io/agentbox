// Command interactive-harness is a manual end-to-end driver for agentbox
// interactive mode. It plays the runner's role: it spawns the agentbox
// binary in interactive mode against a work dir, feeds the lines you type
// as user turns, and prints the streamed assistant output plus the latest
// task-spec. Requires a real agent binary and credentials (e.g.
// ANTHROPIC_API_KEY in the environment, inherited by agentbox).
//
// This is how the undocumented stream-json partial-message shapes get
// validated against the real CLI before they're relied on in production.
//
// Build agentbox first, then run the harness pointing at it and a repo:
//
//	GOWORK=off go build -o /tmp/agentbox ./cmd/agentbox
//	ANTHROPIC_API_KEY=sk-ant-... \
//	  go run ./cmd/interactive-harness -agentbox /tmp/agentbox -workdir /path/to/repo
//
// Type a message and press enter; Ctrl-D ends input and SIGTERMs agentbox.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const planModePrompt = `You are in plan mode for a code repository. Investigate with read-only tools only — you cannot modify, build, run, or test the code (the sandbox is read-only at the OS level). Do not attempt builds or tests; verification happens later when the task is executed — note what should be verified in the spec's acceptance criteria instead.

Each turn, judge what the user is doing:
- Just asking a question, exploring, or discussing — answer normally and DO NOT emit a task-spec block.
- Working toward a concrete code change to dispatch — maintain a task-spec block at the END of your message, refining it as the task firms up.

Only emit a task-spec once the user has expressed intent to change the code; never fabricate a task from a pure question. Block format:

` + "```task-spec" + `
{"title":"...","goal":"...","context":"...","acceptance_criteria":["..."],"assumptions":["..."],"out_of_scope":["..."],"complexity":"low|medium|high","readiness":"vague|partial|ready","readiness_notes":"..."}
` + "```" + `

Set readiness to "ready" only when the goal, acceptance criteria, and file scope are concrete. Set complexity to the model tier the EXECUTION task needs: "low" = trivial/one-file change, "medium" = a few files with some logic, "high" = multi-file work, refactors, tests, or tricky logic. It's a hint for choosing the execution model; the user can override.`

func main() {
	agentboxBin := flag.String("agentbox", "", "path to the built agentbox binary (required)")
	workdir := flag.String("workdir", "", "work dir / repo for the agent to investigate (required)")
	model := flag.String("model", "", "model override (optional)")
	agentType := flag.String("agent", "claude-code", "agent type: claude-code | codex")
	readonly := flag.Bool("readonly", true, "run the agent read-only")
	flag.Parse()

	if *agentboxBin == "" || *workdir == "" {
		fmt.Fprintln(os.Stderr, "usage: interactive-harness -agentbox <bin> -workdir <dir> [-model m] [-readonly=false]")
		os.Exit(2)
	}

	inputDir := filepath.Join(*workdir, ".agentbox-input", "messages")
	outputDir := filepath.Join(*workdir, ".agentbox-output", "messages")
	specPath := filepath.Join(*workdir, ".agentbox-output", "task-spec.json")

	// Start clean: wipe any prior session's dirs so old messages don't
	// replay when re-running against the same workdir. (Real sessions always
	// get fresh dirs; this is a local-testing convenience.)
	_ = os.RemoveAll(filepath.Join(*workdir, ".agentbox-input"))
	_ = os.RemoveAll(filepath.Join(*workdir, ".agentbox-output"))
	must(os.MkdirAll(inputDir, 0o755))
	must(os.MkdirAll(outputDir, 0o755))

	promptPath := filepath.Join(*workdir, ".agentbox-input", "system-prompt.txt")
	must(os.WriteFile(promptPath, []byte(planModePrompt), 0o644))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.Command(*agentboxBin)
	cmd.Env = append(os.Environ(),
		"AGENT_MODE=interactive",
		"AGENT_TYPE="+*agentType,
		"WORK_DIR="+*workdir,
		"SESSION_ID="+mustUUID(),
		"APPEND_SYSTEM_PROMPT_FILE="+promptPath,
	)
	if *readonly {
		cmd.Env = append(cmd.Env, "READ_ONLY=1")
	}
	if *model != "" {
		cmd.Env = append(cmd.Env, "MODEL="+*model)
	}
	cmd.Stderr = os.Stderr // agentbox logs (incl. the human-formatted stream)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to start agentbox: %v\n", err)
		os.Exit(1)
	}

	go tailOutputs(ctx, outputDir, specPath)

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	fmt.Fprintln(os.Stderr, "[harness] type a message and press enter; Ctrl-D to end")
	seq := 0
	for sc.Scan() {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		seq++
		writeInput(inputDir, seq, text)
	}

	fmt.Fprintln(os.Stderr, "[harness] stdin closed; SIGTERM agentbox and waiting…")
	if cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	_ = cmd.Wait()
	cancel()
}

// tailOutputs polls the output dir for new chunk/final files (in seq
// order) and prints them, and prints the task-spec readiness/goal when it
// changes.
func tailOutputs(ctx context.Context, outputDir, specPath string) {
	seen := map[string]bool{}
	var lastSpec string
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		entries, _ := os.ReadDir(outputDir)
		var names []string
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".json") && !seen[e.Name()] {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, n := range names {
			seen[n] = true
			b, err := os.ReadFile(filepath.Join(outputDir, n))
			if err != nil {
				continue
			}
			var rec struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(b, &rec) != nil {
				continue
			}
			switch rec.Type {
			case "chunk":
				fmt.Print(rec.Text)
			case "final":
				fmt.Println("\n[final] ----")
			}
		}

		if b, err := os.ReadFile(specPath); err == nil && string(b) != lastSpec {
			lastSpec = string(b)
			var sr struct {
				Goal       string `json:"goal"`
				Readiness  string `json:"readiness"`
				Complexity string `json:"complexity"`
			}
			_ = json.Unmarshal(b, &sr)
			fmt.Printf("\n[spec] readiness=%s complexity=%s goal=%q\n", sr.Readiness, sr.Complexity, sr.Goal)
		}
	}
}

func writeInput(dir string, seq int, text string) {
	rec := map[string]any{
		"id":      fmt.Sprintf("h-%d", seq),
		"content": text,
		"ts":      time.Now().Unix(),
	}
	b, _ := json.Marshal(rec)
	name := fmt.Sprintf("%010d.json", seq)
	tmp := filepath.Join(dir, name+".tmp")
	_ = os.WriteFile(tmp, b, 0o644)
	_ = os.Rename(tmp, filepath.Join(dir, name))
}

func mustUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
