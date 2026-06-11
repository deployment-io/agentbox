package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/deployment-io/agentbox/internal/agent"
	"github.com/deployment-io/agentbox/internal/config"
)

// codexFakeIO delivers one user turn then EOF, and captures forwarded output.
type codexFakeIO struct {
	msgs   chan agent.UserMessage
	chunks []string
	finals []string
	specs  []agent.SpecSnapshot
}

func (f *codexFakeIO) NextUserMessage(ctx context.Context) (agent.UserMessage, error) {
	select {
	case <-ctx.Done():
		return agent.UserMessage{}, ctx.Err()
	case m, ok := <-f.msgs:
		if !ok {
			return agent.UserMessage{}, io.EOF
		}
		return m, nil
	}
}
func (f *codexFakeIO) Heartbeat(agent.SessionState) error { return nil }
func (f *codexFakeIO) ForwardChunk(c agent.AssistantChunk) error {
	f.chunks = append(f.chunks, c.Text)
	return nil
}
func (f *codexFakeIO) ForwardFinal(m agent.AssistantMessage) error {
	f.finals = append(f.finals, m.Text)
	return nil
}
func (f *codexFakeIO) ForwardSpecUpdate(s agent.SpecSnapshot) error {
	f.specs = append(f.specs, s)
	return nil
}

// TestRunSession_AppServerHandshakeAndTurn runs the codex driver against a
// simulated App Server, asserting the handshake completes and a turn's
// streamed deltas, final message, and task-spec are forwarded.
func TestRunSession_AppServerHandshakeAndTurn(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	fio := &codexFakeIO{msgs: make(chan agent.UserMessage, 1)}
	fio.msgs <- agent.UserMessage{ID: "1", Text: "summarize the repo"}
	close(fio.msgs) // after one turn, NextUserMessage returns io.EOF

	sess := &agent.InteractiveSession{Stdin: stdinW, Stdout: stdoutR, IO: fio, Log: io.Discard}

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		reqs := bufio.NewScanner(stdinR)
		reqs.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		enc := json.NewEncoder(stdoutW)
		readReq := func() bool { return reqs.Scan() }

		readReq() // initialize (id 0)
		_ = enc.Encode(map[string]any{"id": 0, "result": map[string]any{"userAgent": "x"}})
		readReq() // initialized notification (no response)
		readReq() // thread/start (id 1)
		_ = enc.Encode(map[string]any{"id": 1, "result": map[string]any{"thread": map[string]any{"id": "thr_1"}}})
		readReq() // turn/start (id 2)
		_ = enc.Encode(map[string]any{"id": 2, "result": map[string]any{"turn": map[string]any{"id": "t1", "status": "inProgress"}}})
		_ = enc.Encode(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"itemId": "i1", "delta": "Here is "}})
		_ = enc.Encode(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"itemId": "i1", "delta": "the plan."}})
		_ = enc.Encode(map[string]any{"method": "item/completed", "params": map[string]any{"item": map[string]any{
			"type": "agentMessage", "id": "i1",
			"text": "Here is the plan.\n\n```task-spec\n{\"title\":\"T\",\"goal\":\"G\"}\n```",
		}}})
		_ = enc.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"turn": map[string]any{"id": "t1", "status": "completed"}}})

		for reqs.Scan() { // drain until RunSession closes stdin
		}
		_ = stdoutW.Close()
	}()

	errCh := make(chan error, 1)
	go func() {
		errCh <- (&Driver{}).RunSession(context.Background(),
			&config.Config{WorkDir: t.TempDir(), Model: "gpt-5.5", ReadOnly: true}, sess)
	}()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("RunSession: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunSession did not finish")
	}
	<-serverDone

	if strings.Join(fio.chunks, "") != "Here is the plan." {
		t.Errorf("streamed chunks = %v, want [Here is  the plan.]", fio.chunks)
	}
	if len(fio.finals) != 1 || strings.Contains(fio.finals[0], "task-spec") {
		t.Errorf("final should be stripped of the spec block: %v", fio.finals)
	}
	if !strings.Contains(fio.finals[0], "Here is the plan.") {
		t.Errorf("final should keep the prose: %v", fio.finals)
	}
	if len(fio.specs) != 1 || fio.specs[0].Goal != "G" {
		t.Errorf("spec = %v, want one with goal G", fio.specs)
	}
}

// TestBuildInteractiveArgs_AppServer checks the launch argv.
func TestBuildInteractiveArgs_AppServer(t *testing.T) {
	args := (&Driver{}).BuildInteractiveArgs(&config.Config{})
	if len(args) == 0 || args[0] != "app-server" {
		t.Errorf("args should start with app-server: %v", args)
	}
}

// TestSandboxMode pins the sandbox to danger-full-access regardless of the
// read-only flag: read-only / workspace-write enforce via bubblewrap, which
// can't create its namespace inside the container, so commands fail before
// running. The container + proxy are the sandbox.
func TestSandboxMode(t *testing.T) {
	for _, ro := range []bool{true, false} {
		if got := sandboxMode(&config.Config{ReadOnly: ro}); got != "danger-full-access" {
			t.Errorf("sandboxMode(ReadOnly=%v) = %q, want danger-full-access", ro, got)
		}
	}
}

// notifLine builds one JSON-RPC notification line. json.Marshal handles all
// escaping (backticks/quotes in the spec block).
func notifLine(method string, params map[string]any) string {
	b, _ := json.Marshal(map[string]any{"method": method, "params": params})
	return string(b)
}

// TestRunTurn_MultipleAgentMessages verifies Codex's progressive narration
// (multiple agentMessage items per turn) is forwarded as one final per item,
// not collapsed into a single turn-final message.
func TestRunTurn_MultipleAgentMessages(t *testing.T) {
	specText := "Here is the plan.\n\n```task-spec\n{\"title\":\"T\",\"goal\":\"G\"}\n```"
	lines := strings.Join([]string{
		notifLine("item/agentMessage/delta", map[string]any{"itemId": "a", "delta": "Looking… "}),
		notifLine("item/completed", map[string]any{"item": map[string]any{"type": "agentMessage", "id": "a", "text": "Looking… done."}}),
		notifLine("item/agentMessage/delta", map[string]any{"itemId": "b", "delta": "Here is the plan."}),
		notifLine("item/completed", map[string]any{"item": map[string]any{"type": "agentMessage", "id": "b", "text": specText}}),
		notifLine("turn/completed", map[string]any{"turn": map[string]any{"id": "t", "status": "completed"}}),
	}, "\n") + "\n"

	sink := &codexFakeIO{}
	r := newRPCReader(strings.NewReader(lines), io.Discard)
	if err := r.runTurn(sink); err != nil {
		t.Fatalf("runTurn: %v", err)
	}

	if len(sink.finals) != 2 {
		t.Fatalf("expected 2 finals (one per agent message), got %d: %v", len(sink.finals), sink.finals)
	}
	if sink.finals[0] != "Looking… done." {
		t.Errorf("finals[0] = %q, want %q", sink.finals[0], "Looking… done.")
	}
	if sink.finals[1] != "Here is the plan." {
		t.Errorf("finals[1] = %q, want %q (spec stripped)", sink.finals[1], "Here is the plan.")
	}
	if len(sink.specs) != 1 || sink.specs[0].Goal != "G" {
		t.Errorf("specs = %v, want one with goal G", sink.specs)
	}
}
