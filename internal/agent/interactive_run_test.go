package agent

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
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
		pumpUserMessages(context.Background(), encode, fio, w)
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
		pumpUserMessages(ctx, func(string) ([]byte, error) { return nil, nil }, fio, w)
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
		heartbeatLoop(ctx, time.Millisecond, &fakeParser{}, fio)
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
