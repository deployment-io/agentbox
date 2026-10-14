package agent

import (
	"context"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingIO is an inner InteractiveIO that records every sink call in
// order and serves user messages from a channel.
type recordingIO struct {
	msgs chan UserMessage

	mu     sync.Mutex
	events []string
}

func newRecordingIO() *recordingIO { return &recordingIO{msgs: make(chan UserMessage, 4)} }

func (f *recordingIO) record(e string) {
	f.mu.Lock()
	f.events = append(f.events, e)
	f.mu.Unlock()
}

func (f *recordingIO) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func (f *recordingIO) NextUserMessage(ctx context.Context) (UserMessage, error) {
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
func (f *recordingIO) Heartbeat(SessionState) error { f.record("heartbeat"); return nil }
func (f *recordingIO) ForwardChunk(c AssistantChunk) error {
	f.record("chunk:" + c.Text)
	return nil
}
func (f *recordingIO) ForwardFinal(m AssistantMessage) error {
	f.record("final:" + m.Text)
	return nil
}
func (f *recordingIO) ForwardSpecUpdate(s SpecSnapshot) error {
	f.record("spec:" + s.Title)
	return nil
}
func (f *recordingIO) ForwardSpecProblem(p string) error {
	f.record("problem:" + p)
	return nil
}
func (f *recordingIO) ForwardRepoSuggestion(RepoSuggestion) error {
	f.record("suggestion")
	return nil
}
func (f *recordingIO) ForwardTurnEnd() error { f.record("turn_end"); return nil }

const testProblem = "invalid character '#' after array element at line 5, column 27"

func nextMsg(t *testing.T, r *specRepairIO) UserMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	m, err := r.NextUserMessage(ctx)
	if err != nil {
		t.Fatalf("NextUserMessage: %v", err)
	}
	return m
}

func assertEvents(t *testing.T, inner *recordingIO, want ...string) {
	t.Helper()
	if got := inner.got(); !reflect.DeepEqual(got, want) {
		t.Errorf("inner events =\n  %q\nwant\n  %q", got, want)
	}
}

// brokenTurn plays a user turn whose message carries a broken spec block.
func brokenTurn(r *specRepairIO) {
	_ = r.ForwardChunk(AssistantChunk{Text: "answer"})
	_ = r.ForwardSpecProblem(testProblem)
	_ = r.ForwardFinal(AssistantMessage{Text: "answer"})
	_ = r.ForwardTurnEnd()
}

func TestSpecRepair_CleanTurnPassesThrough(t *testing.T) {
	inner := newRecordingIO()
	r := newSpecRepairIO(inner)
	inner.msgs <- UserMessage{ID: "u1", Text: "hi"}

	if m := nextMsg(t, r); m.ID != "u1" {
		t.Errorf("message = %+v, want u1", m)
	}
	_ = r.ForwardChunk(AssistantChunk{Text: "a"})
	_ = r.ForwardSpecUpdate(SpecSnapshot{Title: "T"})
	_ = r.ForwardRepoSuggestion(RepoSuggestion{})
	_ = r.Heartbeat(SessionState{})
	_ = r.ForwardFinal(AssistantMessage{Text: "a"})
	_ = r.ForwardTurnEnd()
	assertEvents(t, inner, "chunk:a", "spec:T", "suggestion", "heartbeat", "final:a", "turn_end")
}

func TestSpecRepair_ProblemHoldsTurnEndAndQueuesCorrection(t *testing.T) {
	inner := newRecordingIO()
	r := newSpecRepairIO(inner)

	brokenTurn(r)
	assertEvents(t, inner, "chunk:answer", "problem:"+testProblem, "final:answer")

	// A user message that arrived meanwhile waits behind the correction.
	inner.msgs <- UserMessage{ID: "u2", Text: "next"}
	m := nextMsg(t, r)
	if m.ID != "spec-repair-1" {
		t.Fatalf("message ID = %q, want spec-repair-1", m.ID)
	}
	want := "Your last message's task-spec block couldn't be read, so the user can't see the spec: " + testProblem +
		`. Re-send the complete task-spec block as valid JSON — inside JSON strings, escape double quotes as \" or use single quotes instead. Don't repeat the rest of your answer.`
	if m.Text != want {
		t.Errorf("correction text =\n  %s\nwant\n  %s", m.Text, want)
	}
	// Never written to the output records.
	assertEvents(t, inner, "chunk:answer", "problem:"+testProblem, "final:answer")

	// Correction fixes it: exactly one turn end, no notice.
	_ = r.ForwardSpecUpdate(SpecSnapshot{Title: "Fixed"})
	_ = r.ForwardTurnEnd()
	assertEvents(t, inner, "chunk:answer", "problem:"+testProblem, "final:answer", "spec:Fixed", "turn_end")

	if m := nextMsg(t, r); m.ID != "u2" {
		t.Errorf("message after correction = %+v, want u2", m)
	}
}

func TestSpecRepair_CorrectionPreemptsBlockedNextUserMessage(t *testing.T) {
	inner := newRecordingIO()
	r := newSpecRepairIO(inner)

	// The caller is already blocked, as PumpTextStdin is during a turn.
	got := make(chan UserMessage, 1)
	go func() {
		m, _ := r.NextUserMessage(context.Background())
		got <- m
	}()
	time.Sleep(20 * time.Millisecond)

	brokenTurn(r)
	select {
	case m := <-got:
		if m.ID != "spec-repair-1" {
			t.Fatalf("blocked call returned %+v, want the correction", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked NextUserMessage never returned the correction")
	}

	// The inner call started by the blocked caller is still pending; the user
	// message it picks up is delivered on the next call, not lost.
	inner.msgs <- UserMessage{ID: "u2"}
	_ = r.ForwardTurnEnd() // correction turn ends (still broken → notice)
	if m := nextMsg(t, r); m.ID != "u2" {
		t.Errorf("next message = %+v, want u2", m)
	}
}

func TestSpecRepair_CorrectionStillBrokenShowsNoticeOnce(t *testing.T) {
	inner := newRecordingIO()
	r := newSpecRepairIO(inner)

	brokenTurn(r)
	if m := nextMsg(t, r); m.ID != "spec-repair-1" {
		t.Fatalf("message = %+v, want the correction", m)
	}
	const second = "invalid character 'x' at line 1, column 3"
	_ = r.ForwardSpecProblem(second)
	_ = r.ForwardTurnEnd()

	notice := "The Assistant's spec couldn't be read (" + second + "), so it isn't shown. Ask the Assistant to re-send the spec."
	assertEvents(t, inner,
		"chunk:answer", "problem:"+testProblem, "final:answer",
		"problem:"+second, "chunk:"+notice, "final:"+notice, "turn_end")

	// No second correction for the same user turn.
	inner.msgs <- UserMessage{ID: "u2"}
	if m := nextMsg(t, r); m.ID != "u2" {
		t.Errorf("message = %+v, want u2 (no second correction)", m)
	}

	// A later user turn with its own problem gets its own correction.
	brokenTurn(r)
	if m := nextMsg(t, r); m.ID != "spec-repair-2" {
		t.Errorf("message = %+v, want spec-repair-2", m)
	}
}

func TestSpecRepair_CorrectionWithoutBlockShowsNotice(t *testing.T) {
	inner := newRecordingIO()
	r := newSpecRepairIO(inner)

	brokenTurn(r)
	nextMsg(t, r)
	_ = r.ForwardFinal(AssistantMessage{Text: "sorry"})
	_ = r.ForwardTurnEnd()

	events := inner.got()
	if n := len(events); n < 3 || events[n-1] != "turn_end" || !strings.HasPrefix(events[n-2], "final:The Assistant's spec couldn't be read ("+testProblem) {
		t.Errorf("events = %q, want the notice then the turn end", events)
	}
}

func TestSpecRepair_ProblemThenValidSpecSameTurn(t *testing.T) {
	inner := newRecordingIO()
	r := newSpecRepairIO(inner)

	_ = r.ForwardSpecProblem(testProblem)
	_ = r.ForwardSpecUpdate(SpecSnapshot{Title: "T"})
	_ = r.ForwardTurnEnd()
	assertEvents(t, inner, "problem:"+testProblem, "spec:T", "turn_end")

	inner.msgs <- UserMessage{ID: "u2"}
	if m := nextMsg(t, r); m.ID != "u2" {
		t.Errorf("message = %+v, want u2 (no correction)", m)
	}
}

func TestSpecRepair_EOFPassesThrough(t *testing.T) {
	inner := newRecordingIO()
	close(inner.msgs)
	r := newSpecRepairIO(inner)
	if _, err := r.NextUserMessage(context.Background()); err != io.EOF {
		t.Errorf("err = %v, want io.EOF", err)
	}
}

func TestSpecRepair_UserMessageDuringBrokenTurnWaitsForCorrection(t *testing.T) {
	inner := newRecordingIO()
	r := newSpecRepairIO(inner)

	// The caller is blocked, as PumpTextStdin is during a turn.
	got := make(chan UserMessage, 2)
	go func() {
		m, _ := r.NextUserMessage(context.Background())
		got <- m
	}()
	time.Sleep(20 * time.Millisecond)

	// The problem is reported, then a user message arrives before the turn
	// ends: it must not reach the agent ahead of the correction.
	_ = r.ForwardSpecProblem(testProblem)
	inner.msgs <- UserMessage{ID: "u2"}
	select {
	case m := <-got:
		t.Fatalf("NextUserMessage returned %+v before the broken turn ended", m)
	case <-time.After(50 * time.Millisecond):
	}

	_ = r.ForwardTurnEnd()
	select {
	case m := <-got:
		if m.ID != "spec-repair-1" {
			t.Fatalf("message = %+v, want the correction", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("NextUserMessage never returned the correction")
	}

	// u2 is held until the correction turn ends.
	go func() {
		m, _ := r.NextUserMessage(context.Background())
		got <- m
	}()
	select {
	case m := <-got:
		t.Fatalf("NextUserMessage returned %+v during the correction turn", m)
	case <-time.After(50 * time.Millisecond):
	}
	_ = r.ForwardSpecUpdate(SpecSnapshot{Title: "Fixed"})
	_ = r.ForwardTurnEnd()
	select {
	case m := <-got:
		if m.ID != "u2" {
			t.Fatalf("message = %+v, want u2", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("u2 never delivered after the correction turn")
	}
	assertEvents(t, inner, "problem:"+testProblem, "spec:Fixed", "turn_end")
}

func TestSpecRepair_FinishReleasesHeldTurnEnd(t *testing.T) {
	inner := newRecordingIO()
	r := newSpecRepairIO(inner)

	// The agent broke the spec and then exited (e.g. budget limit) before
	// the correction turn could run.
	brokenTurn(r)
	r.finish()
	notice := "The Assistant's spec couldn't be read (" + testProblem + "), so it isn't shown. Ask the Assistant to re-send the spec."
	assertEvents(t, inner,
		"chunk:answer", "problem:"+testProblem, "final:answer",
		"chunk:"+notice, "final:"+notice, "turn_end")

	// Idempotent, and a late turn end from the driver is not held.
	r.finish()
	_ = r.ForwardTurnEnd()
	if n := len(inner.got()); n != 7 {
		t.Errorf("events = %q, want one more turn_end only", inner.got())
	}
}

func TestSpecRepair_FinishAfterFixedCorrectionInFlight(t *testing.T) {
	inner := newRecordingIO()
	r := newSpecRepairIO(inner)

	brokenTurn(r)
	nextMsg(t, r)
	_ = r.ForwardSpecUpdate(SpecSnapshot{Title: "Fixed"})
	r.finish() // session ended before the correction turn's end
	assertEvents(t, inner, "chunk:answer", "problem:"+testProblem, "final:answer", "spec:Fixed", "turn_end")
}

func TestSpecRepair_FinishCleanSessionIsNoop(t *testing.T) {
	inner := newRecordingIO()
	r := newSpecRepairIO(inner)
	_ = r.ForwardFinal(AssistantMessage{Text: "a"})
	_ = r.ForwardTurnEnd()
	r.finish()
	assertEvents(t, inner, "final:a", "turn_end")
}

func TestSpecRepair_UserMessageNotReleasedByValidBlockBeforeBrokenOne(t *testing.T) {
	inner := newRecordingIO()
	r := newSpecRepairIO(inner)

	got := make(chan UserMessage, 2)
	go func() {
		m, _ := r.NextUserMessage(context.Background())
		got <- m
	}()
	time.Sleep(20 * time.Millisecond)

	// A problem is reported, a user message arrives, then a later message in
	// the same turn carries an older valid block and a newer broken one:
	// the update briefly clears the mark, which must not release u2.
	_ = r.ForwardSpecProblem(testProblem)
	inner.msgs <- UserMessage{ID: "u2"}
	time.Sleep(20 * time.Millisecond)
	_ = r.ForwardSpecUpdate(SpecSnapshot{Title: "Old"})
	time.Sleep(20 * time.Millisecond)
	_ = r.ForwardSpecProblem(testProblem)
	select {
	case m := <-got:
		t.Fatalf("NextUserMessage returned %+v before the broken turn ended", m)
	case <-time.After(50 * time.Millisecond):
	}

	_ = r.ForwardTurnEnd()
	select {
	case m := <-got:
		if m.ID != "spec-repair-1" {
			t.Fatalf("message = %+v, want the correction", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("NextUserMessage never returned the correction")
	}
}

func TestSpecRepair_UserMessageReleasedWhenFixedTurnEnds(t *testing.T) {
	inner := newRecordingIO()
	r := newSpecRepairIO(inner)

	got := make(chan UserMessage, 1)
	go func() {
		m, _ := r.NextUserMessage(context.Background())
		got <- m
	}()
	_ = r.ForwardSpecProblem(testProblem)
	inner.msgs <- UserMessage{ID: "u2"}
	_ = r.ForwardSpecUpdate(SpecSnapshot{Title: "T"})
	_ = r.ForwardTurnEnd()
	select {
	case m := <-got:
		if m.ID != "u2" {
			t.Fatalf("message = %+v, want u2", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("u2 never delivered after the fixed turn ended")
	}
	assertEvents(t, inner, "problem:"+testProblem, "spec:T", "turn_end")
}
