package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// scriptRun is one invocation of the browser script.
type scriptRun struct {
	node      string
	script    string
	args      []string
	timeout   time.Duration // SIGTERM after this long
	grace     time.Duration // then kill after this long
	stderrMax int
}

// scriptOutcome is how the script ended.
type scriptOutcome struct {
	err        error // exit error, nil on exit 0
	timedOut   bool  // ran past its timeout
	cancelled  bool  // this program was told to stop
	stderrTail string
}

// runScript runs the script to completion, bounded by run.timeout: SIGTERM at
// the deadline (or when ctx is cancelled), SIGKILL run.grace later. Its
// stderr is copied to ours and its tail kept.
func runScript(ctx context.Context, run scriptRun) scriptOutcome {
	tctx, cancel := context.WithTimeout(ctx, run.timeout)
	defer cancel()
	cmd := exec.CommandContext(tctx, run.node, append([]string{run.script}, run.args...)...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = run.grace
	tail := &tailBuffer{max: run.stderrMax}
	cmd.Stdout = os.Stderr
	cmd.Stderr = io.MultiWriter(os.Stderr, tail)
	err := cmd.Run()
	out := scriptOutcome{err: err, stderrTail: tail.String()}
	if errors.Is(tctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		out.timedOut = true
	} else if ctx.Err() != nil {
		out.cancelled = true
	}
	return out
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append([]byte(nil), t.buf[over:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
