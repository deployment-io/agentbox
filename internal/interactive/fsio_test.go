package interactive

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

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
	_ = f.ForwardTurnEnd()

	var names []string
	entries, _ := os.ReadDir(f.outputDir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) != 4 {
		t.Fatalf("expected 4 output files, got %d", len(names))
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
	if recs[3].Type != "turn_end" || recs[3].Text != "" {
		t.Errorf("rec3 = %+v, want a bare turn_end record after the final", recs[3])
	}
	if !(recs[0].Seq < recs[1].Seq && recs[1].Seq < recs[2].Seq && recs[2].Seq < recs[3].Seq) {
		t.Errorf("seqs not increasing: %d %d %d %d", recs[0].Seq, recs[1].Seq, recs[2].Seq, recs[3].Seq)
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

// TestWriteJSONAtomic_ConcurrentSamePath hammers one path from many
// goroutines. With a shared "<path>.tmp" name this races — writers clobber the
// single temp file and a rename hits ENOENT after another renamed it away — so
// it guards the unique-temp-name invariant: every write must succeed and the
// final file must be one writer's complete, valid JSON.
func TestWriteJSONAtomic_ConcurrentSamePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spec.json")
	const writers, iters = 16, 60
	var wg sync.WaitGroup
	errs := make(chan error, writers*iters)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				if err := writeJSONAtomic(path, specRecord{Title: fmt.Sprintf("w%d-i%d", w, i)}); err != nil {
					errs <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent writeJSONAtomic: %v", err)
	}
	// No leftover temp files, and the survivor is complete valid JSON.
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	if err := json.Unmarshal(b, &specRecord{}); err != nil {
		t.Fatalf("final file is not complete valid JSON (%v): %q", err, b)
	}
}

// TestFSIO_ConcurrentDuplex drives one FSIO from the full production goroutine
// topology at once — an input producer + a NextUserMessage consumer + several
// Forward* writers + a Heartbeat loop — so `go test -race` exercises the
// struct's concurrent surface. The atomic seq counter must hand every
// writeOutput a distinct value, so the output file count equals the number of
// chunk+final calls with no duplicate seqs, and every file is valid JSON.
func TestFSIO_ConcurrentDuplex(t *testing.T) {
	f, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.poll = time.Millisecond // spin NextUserMessage fast

	const producers, writers, iters = 1, 4, 80
	const wantOutputs = writers * iters * 2 // chunk + final per iter

	// Consumer: drain user messages until cancelled.
	ctx, cancel := context.WithCancel(context.Background())
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		for {
			if _, err := f.NextUserMessage(ctx); err != nil {
				return
			}
		}
	}()

	var wg sync.WaitGroup
	// Producer drops input files atomically (the runner's role). writeJSONAtomic
	// names temps "<n>.json.*.tmp", which the consumer skips (it takes *.json).
	wg.Add(producers)
	go func() {
		defer wg.Done()
		for i := 0; i < writers*iters; i++ {
			_ = writeJSONAtomic(filepath.Join(f.inputDir, fmt.Sprintf("%06d.json", i)),
				inputMessage{ID: fmt.Sprintf("u%d", i), Content: "hi"})
		}
	}()
	// Several concurrent Forward* writers — stresses the atomic seq under
	// contention (production serializes these; FSIO is safe either way).
	wg.Add(writers)
	for w := 0; w < writers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				_ = f.ForwardChunk(agent.AssistantChunk{Text: "delta"})
				_ = f.ForwardFinal(agent.AssistantMessage{Text: "done"})
				_ = f.ForwardSpecUpdate(agent.SpecSnapshot{Title: "t", Raw: "{}"})
			}
		}()
	}
	// Heartbeat on its own goroutine, as in production.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			_ = f.Heartbeat(agent.SessionState{Turns: i})
		}
	}()
	wg.Wait()
	cancel()
	<-consumerDone

	// Every output file is valid JSON with a unique seq, and the count matches
	// the number of writeOutput calls (no seq collision lost a file).
	entries, err := os.ReadDir(f.outputDir)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[int64]bool)
	count := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		count++
		b, err := os.ReadFile(filepath.Join(f.outputDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		var r outputRecord
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatalf("output %s not valid JSON (%v): %q", e.Name(), err, b)
		}
		if seen[r.Seq] {
			t.Errorf("duplicate seq %d", r.Seq)
		}
		seen[r.Seq] = true
	}
	if count != wantOutputs {
		t.Errorf("output file count = %d, want %d (seq collision dropped a file?)", count, wantOutputs)
	}
	// Spec + heartbeat files must parse too.
	if b, err := os.ReadFile(f.specPath); err != nil {
		t.Errorf("spec read: %v", err)
	} else if err := json.Unmarshal(b, &specRecord{}); err != nil {
		t.Errorf("spec not valid JSON: %v", err)
	}
	if b, err := os.ReadFile(f.hbPath); err != nil {
		t.Errorf("heartbeat read: %v", err)
	} else if err := json.Unmarshal(b, &heartbeatRecord{}); err != nil {
		t.Errorf("heartbeat not valid JSON: %v", err)
	}
}
