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

func TestSandboxMode(t *testing.T) {
	if got := sandboxMode(&config.Config{ReadOnly: true}); got != "readOnly" {
		t.Errorf("read-only sandbox = %q, want readOnly", got)
	}
	if got := sandboxMode(&config.Config{ReadOnly: false}); got != "workspaceWrite" {
		t.Errorf("non-read-only sandbox = %q, want workspaceWrite", got)
	}
}
