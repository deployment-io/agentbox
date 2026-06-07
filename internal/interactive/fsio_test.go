package interactive

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/agent"
)

func writeInputFile(t *testing.T, dir, name string, msg inputMessage) {
	t.Helper()
	b, _ := json.Marshal(msg)
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
		t.Fatalf("write input: %v", err)
	}
}

func TestFSIO_NextUserMessage(t *testing.T) {
	f, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeInputFile(t, f.inputDir, "001.json", inputMessage{ID: "a", Content: "hello"})

	msg, err := f.NextUserMessage(context.Background())
	if err != nil {
		t.Fatalf("NextUserMessage: %v", err)
	}
	if msg.ID != "a" || msg.Text != "hello" {
		t.Errorf("msg = %+v", msg)
	}
	entries, _ := os.ReadDir(f.inputDir)
	if len(entries) != 0 {
		t.Errorf("input file should be consumed, found %d", len(entries))
	}
}

func TestFSIO_NextUserMessageOrder(t *testing.T) {
	f, _ := New(t.TempDir())
	writeInputFile(t, f.inputDir, "002.json", inputMessage{ID: "b", Content: "second"})
	writeInputFile(t, f.inputDir, "001.json", inputMessage{ID: "a", Content: "first"})

	m1, _ := f.NextUserMessage(context.Background())
	m2, _ := f.NextUserMessage(context.Background())
	if m1.Text != "first" || m2.Text != "second" {
		t.Errorf("order wrong: %q then %q", m1.Text, m2.Text)
	}
}

func TestFSIO_NextUserMessageContextCancel(t *testing.T) {
	f, _ := New(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.NextUserMessage(ctx); err == nil {
		t.Error("expected error on cancelled context with no input")
	}
}

func TestFSIO_NextUserMessageFallbackID(t *testing.T) {
	f, _ := New(t.TempDir())
	writeInputFile(t, f.inputDir, "msg-7.json", inputMessage{Content: "no id field"})
	msg, err := f.NextUserMessage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if msg.ID != "msg-7" {
		t.Errorf("ID should fall back to filename stem, got %q", msg.ID)
	}
}

func TestFSIO_OutputOrdering(t *testing.T) {
	f, _ := New(t.TempDir())
	_ = f.ForwardChunk(agent.AssistantChunk{Text: "Hel"})
	_ = f.ForwardChunk(agent.AssistantChunk{Text: "lo"})
	_ = f.ForwardFinal(agent.AssistantMessage{Text: "Hello"})

	var names []string
	entries, _ := os.ReadDir(f.outputDir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) != 3 {
		t.Fatalf("expected 3 output files, got %d", len(names))
	}

	var recs []outputRecord
	for _, n := range names {
		b, _ := os.ReadFile(filepath.Join(f.outputDir, n))
		var r outputRecord
		_ = json.Unmarshal(b, &r)
		recs = append(recs, r)
	}
	if recs[0].Type != "chunk" || recs[0].Text != "Hel" {
		t.Errorf("rec0 = %+v", recs[0])
	}
	if recs[2].Type != "final" || recs[2].Text != "Hello" {
		t.Errorf("rec2 = %+v", recs[2])
	}
	if !(recs[0].Seq < recs[1].Seq && recs[1].Seq < recs[2].Seq) {
		t.Errorf("seqs not increasing: %d %d %d", recs[0].Seq, recs[1].Seq, recs[2].Seq)
	}
}

func TestFSIO_SpecAndHeartbeat(t *testing.T) {
	f, _ := New(t.TempDir())
	if err := f.ForwardSpecUpdate(agent.SpecSnapshot{Goal: "do X", Readiness: "ready", Raw: "{}"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(f.specPath)
	if err != nil {
		t.Fatalf("spec file: %v", err)
	}
	var sr specRecord
	_ = json.Unmarshal(b, &sr)
	if sr.Goal != "do X" || sr.Readiness != "ready" {
		t.Errorf("spec = %+v", sr)
	}

	if err := f.Heartbeat(agent.SessionState{Turns: 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.hbPath); err != nil {
		t.Errorf("heartbeat file missing: %v", err)
	}
}
