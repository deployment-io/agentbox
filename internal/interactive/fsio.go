// Package interactive provides a filesystem-backed agent.InteractiveIO:
// the runner-facing side of an interactive session. User turns arrive as
// JSON files the runner drops into an input directory; the agent's
// streamed output, the latest task-spec, and a liveness heartbeat are
// written back as JSON files the runner watches.
//
// Directory layout under the work dir:
//
//	.agentbox-input/messages/<name>.json    user turns (consumed in name order)
//	.agentbox-output/messages/<seq>.json    assistant chunks + finals (seq order)
//	.agentbox-output/task-spec.json         latest extracted spec (overwritten)
//	.agentbox-output/heartbeat.json         latest liveness snapshot (overwritten)
//
// Both sides write atomically (temp file + rename) so the reader never
// observes a partial file.
package interactive

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/deployment-io/agentbox/internal/agent"
)

// Compile-time check that FSIO satisfies the full duplex interface.
var _ agent.InteractiveIO = (*FSIO)(nil)

const defaultPollInterval = 500 * time.Millisecond

// FSIO is the filesystem implementation of agent.InteractiveIO.
type FSIO struct {
	inputDir  string
	outputDir string
	specPath  string
	hbPath    string
	poll      time.Duration
	seq       atomic.Int64
}

// New creates the input/output directories under workDir and returns an
// FSIO ready to drive a session.
func New(workDir string) (*FSIO, error) {
	outBase := filepath.Join(workDir, ".agentbox-output")
	f := &FSIO{
		inputDir:  filepath.Join(workDir, ".agentbox-input", "messages"),
		outputDir: filepath.Join(outBase, "messages"),
		specPath:  filepath.Join(outBase, "task-spec.json"),
		hbPath:    filepath.Join(outBase, "heartbeat.json"),
		poll:      defaultPollInterval,
	}
	if err := os.MkdirAll(f.inputDir, 0o755); err != nil {
		return nil, fmt.Errorf("create input dir: %w", err)
	}
	if err := os.MkdirAll(f.outputDir, 0o755); err != nil {
		return nil, fmt.Errorf("create output dir: %w", err)
	}
	return f, nil
}

type inputMessage struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Ts      int64  `json:"ts"`
}

// NextUserMessage returns the oldest pending input file (by filename),
// consuming it, or blocks polling until one appears or ctx is cancelled.
func (f *FSIO) NextUserMessage(ctx context.Context) (agent.UserMessage, error) {
	ticker := time.NewTicker(f.poll)
	defer ticker.Stop()
	for {
		if msg, ok, err := f.takeOldestInput(); err != nil {
			return agent.UserMessage{}, err
		} else if ok {
			return msg, nil
		}
		select {
		case <-ctx.Done():
			return agent.UserMessage{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

// takeOldestInput reads, consumes, and returns the lexically-first input
// file. ok=false means the directory is currently empty.
func (f *FSIO) takeOldestInput() (agent.UserMessage, bool, error) {
	entries, err := os.ReadDir(f.inputDir)
	if err != nil {
		if os.IsNotExist(err) {
			return agent.UserMessage{}, false, nil
		}
		return agent.UserMessage{}, false, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		path := filepath.Join(f.inputDir, name)
		b, err := os.ReadFile(path)
		if err != nil {
			continue // transient (e.g. removed concurrently); try next poll
		}
		var in inputMessage
		if jsonErr := json.Unmarshal(b, &in); jsonErr != nil {
			// The runner writes atomically, so a present .json is complete;
			// a parse failure is genuine corruption — drop it and continue.
			_ = os.Remove(path)
			fmt.Fprintf(os.Stderr, "[agentbox] dropping unparseable input %s: %v\n", name, jsonErr)
			continue
		}
		_ = os.Remove(path)
		id := in.ID
		if id == "" {
			id = strings.TrimSuffix(name, ".json")
		}
		return agent.UserMessage{ID: id, Text: in.Content}, true, nil
	}
	return agent.UserMessage{}, false, nil
}

type outputRecord struct {
	Seq  int64  `json:"seq"`
	Type string `json:"type"` // "chunk" | "final" | "turn_end"
	Text string `json:"text"`
}

// ForwardChunk writes an incremental assistant-text delta.
func (f *FSIO) ForwardChunk(c agent.AssistantChunk) error {
	return f.writeOutput(outputRecord{Type: "chunk", Text: c.Text})
}

// ForwardFinal writes the completed assistant message for a turn.
func (f *FSIO) ForwardFinal(m agent.AssistantMessage) error {
	return f.writeOutput(outputRecord{Type: "final", Text: m.Text})
}

// ForwardTurnEnd writes the turn-boundary record: the agent finished its
// turn and is blocked waiting for the next user message.
func (f *FSIO) ForwardTurnEnd() error {
	return f.writeOutput(outputRecord{Type: "turn_end"})
}

func (f *FSIO) writeOutput(rec outputRecord) error {
	rec.Seq = f.seq.Add(1)
	// Zero-padded so a lexical sort of the directory is chronological.
	name := fmt.Sprintf("%010d.json", rec.Seq)
	return writeJSONAtomic(filepath.Join(f.outputDir, name), rec)
}

type specRecord struct {
	Title       string   `json:"title"`
	Goal        string   `json:"goal"`
	Context     string   `json:"context"`
	Acceptance  []string `json:"acceptance_criteria"`
	Assumptions []string `json:"assumptions"`
	OutOfScope  []string `json:"out_of_scope"`
	Readiness   string   `json:"readiness"`
	Notes       string   `json:"readiness_notes"`
	Complexity  string   `json:"complexity"`
	Raw         string   `json:"raw"`
}

// ForwardSpecUpdate overwrites the latest-spec file. The runner picks it
// up the same way it polls progress.json today.
func (f *FSIO) ForwardSpecUpdate(s agent.SpecSnapshot) error {
	return writeJSONAtomic(f.specPath, specRecord{
		Title:       s.Title,
		Goal:        s.Goal,
		Context:     s.Context,
		Acceptance:  s.Acceptance,
		Assumptions: s.Assumptions,
		OutOfScope:  s.OutOfScope,
		Readiness:   s.Readiness,
		Notes:       s.Notes,
		Complexity:  s.Complexity,
		Raw:         s.Raw,
	})
}

type heartbeatRecord struct {
	Ts           int64 `json:"ts"`
	Turns        int   `json:"turns"`
	InputTokens  int   `json:"input_tokens"`
	OutputTokens int   `json:"output_tokens"`
}

// Heartbeat overwrites the liveness file with the latest snapshot.
func (f *FSIO) Heartbeat(s agent.SessionState) error {
	return writeJSONAtomic(f.hbPath, heartbeatRecord{
		Ts:           time.Now().Unix(),
		Turns:        s.Turns,
		InputTokens:  s.InputTokens,
		OutputTokens: s.OutputTokens,
	})
}

// writeJSONAtomic marshals v and writes it via a uniquely-named temp file in
// the same directory, then renames it over path so a reader never sees a
// partially-written file. The temp name is unique (os.CreateTemp), not a
// shared path+".tmp": concurrent writers — to different logical paths, or
// (defensively) the same one — never clobber each other's temp or race on the
// rename. Same-directory keeps the rename atomic (one filesystem).
func writeJSONAtomic(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	// CreateTemp makes the file 0o600; match the previous 0o644 so the runner
	// (reading these back) sees the same perms.
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}
