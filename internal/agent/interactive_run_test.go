package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/deployment-io/agentbox/internal/config"
	"github.com/deployment-io/agentbox/internal/result"
)

// fakeIO is an in-memory InteractiveIO for the pump/heartbeat tests.
// (fakeDriver and fakeParser are defined in driver_test.go.)
type fakeIO struct {
	msgs chan UserMessage

	mu    sync.Mutex
	beats int
}

func (f *fakeIO) NextUserMessage(ctx context.Context) (UserMessage, error) {
	select {
	case <-ctx.Done():
		return UserMessage{}, ctx.Err()
	case m, ok := <-f.msgs:
		if !ok {
			return UserMessage{}, io.EOF
		}
		return m, nil
	}
}

func (f *fakeIO) Heartbeat(SessionState) error {
	f.mu.Lock()
	f.beats++
	f.mu.Unlock()
	return nil
}

func (f *fakeIO) heartbeats() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.beats
}

func (f *fakeIO) ForwardChunk(AssistantChunk) error    { return nil }
func (f *fakeIO) ForwardFinal(AssistantMessage) error  { return nil }
func (f *fakeIO) ForwardSpecUpdate(SpecSnapshot) error { return nil }
func (f *fakeIO) ForwardTurnEnd() error                { return nil }

// recordWriteCloser captures everything written and tracks Close.
type recordWriteCloser struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	closed bool
}

func (w *recordWriteCloser) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *recordWriteCloser) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	return nil
}

func (w *recordWriteCloser) contents() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func (w *recordWriteCloser) isClosed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closed
}

func TestPumpUserMessages(t *testing.T) {
	fio := &fakeIO{msgs: make(chan UserMessage, 2)}
	fio.msgs <- UserMessage{ID: "1", Text: "first"}
	fio.msgs <- UserMessage{ID: "2", Text: "second"}
	close(fio.msgs) // drained, then NextUserMessage returns io.EOF

	w := &recordWriteCloser{}
	encode := func(s string) ([]byte, error) { return []byte("MSG:" + s + "\n"), nil }

	done := make(chan struct{})
	go func() {
		PumpTextStdin(context.Background(), fio, encode, w)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not return after EOF")
	}

	if got := w.contents(); got != "MSG:first\nMSG:second\n" {
		t.Errorf("stdin content = %q", got)
	}
	if !w.isClosed() {
		t.Error("pump should close stdin on EOF")
	}
}

func TestPumpUserMessages_StopsOnContextCancel(t *testing.T) {
	fio := &fakeIO{msgs: make(chan UserMessage)} // never delivers
	w := &recordWriteCloser{}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		PumpTextStdin(ctx, fio, func(string) ([]byte, error) { return nil, nil }, w)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not return on ctx cancel")
	}
	if !w.isClosed() {
		t.Error("pump should close stdin when ctx is cancelled")
	}
}

func TestHeartbeatLoop(t *testing.T) {
	fio := &fakeIO{msgs: make(chan UserMessage)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunHeartbeat(ctx, time.Millisecond, fio, func() SessionState { return SessionState{Turns: 1} })
		close(done)
	}()
	time.Sleep(25 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat loop did not stop on ctx cancel")
	}
	if fio.heartbeats() == 0 {
		t.Error("expected at least one heartbeat")
	}
}

func TestRunInteractive_RejectsNonInteractiveDriver(t *testing.T) {
	// fakeDriver implements Driver but not InteractiveDriver.
	fio := &fakeIO{msgs: make(chan UserMessage)}
	out := RunInteractive(context.Background(), &config.Config{AgentType: "fake"}, &fakeDriver{}, fio)
	if out.Status != result.StatusFailure {
		t.Errorf("status = %v, want failure", out.Status)
	}
	if !strings.Contains(out.Error, "interactive") {
		t.Errorf("error should mention interactive: %q", out.Error)
	}
}

// fakeInteractiveDriver runs a real subprocess (sh) and mimics the shipped
// drivers' clean path: drain stdout to EOF, return nil. Lets the lifecycle
// tests exercise the agent-died-vs-we-killed-it classification end to end.
type fakeInteractiveDriver struct {
	fakeDriver
	binary string
	args   []string
}

func (d *fakeInteractiveDriver) Binary() string                               { return d.binary }
func (d *fakeInteractiveDriver) BuildInteractiveArgs(*config.Config) []string { return d.args }
func (d *fakeInteractiveDriver) RunSession(ctx context.Context, cfg *config.Config, sess *InteractiveSession) error {
	_, _ = io.Copy(io.Discard, sess.Stdout) // EOF when the process exits
	return nil
}

// exitErrorWithCode harvests a real *exec.ExitError carrying the given
// self-exit code.
func exitErrorWithCode(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	if err == nil {
		t.Fatalf("expected exit error for code %d", code)
	}
	return err
}

// signalExitError harvests the wait error of a process we SIGTERMed —
// what terminate() produces when winding down a still-live agent.
func signalExitError(t *testing.T) error {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	err := cmd.Wait()
	if err == nil {
		t.Fatal("expected wait error after SIGTERM")
	}
	return err
}

// TestClassifySessionEnd pins the agent-died-vs-we-killed-it distinction: a
// clean driver return only classifies success when the process did not die
// on its own with a real failure code. Our wind-down SIGTERM (signal exit,
// ExitCode -1) must keep classifying success — every healthy session ends
// with one.
func TestClassifySessionEnd(t *testing.T) {
	tests := []struct {
		name    string
		serr    error
		waitErr error
		want    result.Status
		errHas  string
	}{
		{name: "clean end, process exited 0", serr: nil, waitErr: nil, want: result.StatusSuccess},
		{name: "clean end, we SIGTERMed it", serr: nil, waitErr: signalExitError(t), want: result.StatusSuccess},
		{name: "clean end, agent died with exit 7", serr: nil, waitErr: exitErrorWithCode(t, 7), want: result.StatusFailure, errHas: "exited with error"},
		{name: "protocol error wins regardless", serr: fmt.Errorf("codex app-server error: boom"), waitErr: nil, want: result.StatusFailure, errHas: "boom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := classifySessionEnd(tt.serr, tt.waitErr, "stderr tail", "fake-agent")
			if out.Status != tt.want {
				t.Fatalf("status = %v, want %v (error %q)", out.Status, tt.want, out.Error)
			}
			if tt.errHas != "" && !strings.Contains(out.Error, tt.errHas) {
				t.Errorf("error %q should contain %q", out.Error, tt.errHas)
			}
		})
	}
}

// TestRunInteractive_AgentSelfExitFailure reproduces the misclassification
// seen live: the agent prints a startup error and exits non-zero; the
// driver's reader EOFs and returns nil. Whichever select arm wins the
// done-vs-sessionDone race, the outcome must be failure with the stderr
// detail — this read "status=success" before the classify fix.
func TestRunInteractive_AgentSelfExitFailure(t *testing.T) {
	d := &fakeInteractiveDriver{binary: "sh", args: []string{"-c", "echo boom >&2; exit 7"}}
	fio := &fakeIO{msgs: make(chan UserMessage)}
	out := RunInteractive(context.Background(), &config.Config{AgentType: "fake", WorkDir: t.TempDir()}, d, fio)
	if out.Status != result.StatusFailure {
		t.Fatalf("status = %v, want failure (error %q)", out.Status, out.Error)
	}
	if !strings.Contains(out.Error, "boom") {
		t.Errorf("error should carry the stderr tail: %q", out.Error)
	}
}

func TestRunInteractive_AgentCleanExitSuccess(t *testing.T) {
	d := &fakeInteractiveDriver{binary: "sh", args: []string{"-c", "exit 0"}}
	fio := &fakeIO{msgs: make(chan UserMessage)}
	out := RunInteractive(context.Background(), &config.Config{AgentType: "fake", WorkDir: t.TempDir()}, d, fio)
	if out.Status != result.StatusSuccess {
		t.Fatalf("status = %v, want success (error %q)", out.Status, out.Error)
	}
}
