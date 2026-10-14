package agent

import (
	"context"
	"fmt"
	"os"
	"sync"
)

// specRepairIO wraps a session's InteractiveIO so that a task-spec block
// the agent broke (ForwardSpecProblem) doesn't fail silently. RunInteractive
// puts it around every session's IO, so it is driver-agnostic: it sees only
// the sink calls and NextUserMessage, never a wire protocol.
//
// A turn that reported a spec problem, and didn't follow it with a valid
// spec, has its turn end held: instead, one correction turn is queued for
// the agent (returned by the next NextUserMessage, ahead of any user
// message). When the correction turn ends, the held turn end is forwarded —
// preceded by a visible notice if the spec is still unreadable. At most one
// correction per user turn. Once a turn reports a spec problem, user
// messages are held back until that turn ends (or, if it queues a
// correction, until the correction turn ends), so none reaches the agent
// ahead of the correction — even if a valid spec briefly clears the mark. If the session ends while a
// turn end is held, finish releases it. A turn with no spec problem passes
// every call straight through, in order.
type specRepairIO struct {
	InteractiveIO

	// repairs carries a queued correction turn to NextUserMessage. Capacity
	// 1: at most one correction is outstanding (repairing gates it).
	repairs chan UserMessage

	mu sync.Mutex
	// unreadable is set by ForwardSpecProblem and cleared by
	// ForwardSpecUpdate; problem is the latest problem reported.
	unreadable bool
	problem    string
	// problemSeen is set by ForwardSpecProblem and cleared only at a turn
	// end, so a ForwardSpecUpdate in between (e.g. an older valid block
	// forwarded just before a newer broken one) never releases a held user
	// message mid-turn.
	problemSeen bool
	// repairing is true from queueing a correction until its turn ends.
	repairing bool
	// count numbers correction turns for their IDs.
	count int
	// changed is closed (and replaced) whenever problemSeen or repairing
	// changes, waking a NextUserMessage that is holding a user message.
	changed chan struct{}
	// next is an in-flight (or finished, unconsumed) call to the inner
	// NextUserMessage, kept across calls so a user message that arrives
	// while a correction is returned is not lost.
	next *pendingMessage
}

type pendingMessage struct {
	ctx  context.Context
	done chan struct{}
	msg  UserMessage
	err  error
}

func newSpecRepairIO(inner InteractiveIO) *specRepairIO {
	return &specRepairIO{InteractiveIO: inner, repairs: make(chan UserMessage, 1), changed: make(chan struct{})}
}

// correctionText is the correction turn sent to the agent.
func correctionText(problem string) string {
	return "Your last message's task-spec block couldn't be read, so the user can't see the spec: " + problem +
		". Re-send the complete task-spec block as valid JSON — inside JSON strings, escape double quotes as \\\" or use single quotes instead. Don't repeat the rest of your answer."
}

// noticeText is the assistant message shown when the correction didn't help.
func noticeText(problem string) string {
	return "The Assistant's spec couldn't be read (" + problem + "), so it isn't shown. Ask the Assistant to re-send the spec."
}

// signal wakes waiters on changed. Callers hold mu.
func (r *specRepairIO) signal() {
	close(r.changed)
	r.changed = make(chan struct{})
}

func (r *specRepairIO) ForwardSpecUpdate(s SpecSnapshot) error {
	r.mu.Lock()
	r.unreadable = false
	r.mu.Unlock()
	return r.InteractiveIO.ForwardSpecUpdate(s)
}

func (r *specRepairIO) ForwardSpecProblem(problem string) error {
	r.mu.Lock()
	r.unreadable = true
	r.problemSeen = true
	r.problem = problem
	r.signal()
	r.mu.Unlock()
	return r.InteractiveIO.ForwardSpecProblem(problem)
}

func (r *specRepairIO) ForwardTurnEnd() error {
	r.mu.Lock()
	if r.repairing {
		// The correction turn ended: release the held turn end.
		return r.release()
	}
	if !r.unreadable {
		if r.problemSeen {
			// Fixed within the turn: release any held user message.
			r.problemSeen = false
			r.signal()
		}
		r.mu.Unlock()
		return r.InteractiveIO.ForwardTurnEnd()
	}
	// A user turn ended with an unreadable spec: hold its turn end and queue
	// one correction turn. unreadable stays set until a valid spec arrives.
	r.repairing = true
	r.signal()
	r.count++
	msg := UserMessage{ID: fmt.Sprintf("spec-repair-%d", r.count), Text: correctionText(r.problem)}
	r.mu.Unlock()
	fmt.Fprintf(os.Stderr, "[agentbox] task-spec unreadable; holding the turn end and sending correction turn %s\n", msg.ID)
	select {
	case r.repairs <- msg:
	default:
		// An earlier correction is still unconsumed; it asks for the same
		// fix, so don't queue (or block on) a second one.
	}
	return nil
}

// release ends a correction turn: it forwards the held turn end, with a
// notice first if the spec is still unreadable. No second try. Called with mu
// held and repairing set; it unlocks mu.
func (r *specRepairIO) release() error {
	r.repairing = false
	notice, problem := r.unreadable, r.problem
	r.unreadable, r.problemSeen, r.problem = false, false, ""
	r.signal()
	// A correction the agent never received is moot now.
	select {
	case <-r.repairs:
	default:
	}
	r.mu.Unlock()
	if notice {
		fmt.Fprintf(os.Stderr, "[agentbox] task-spec still unreadable after the correction turn; showing a notice\n")
		text := noticeText(problem)
		_ = r.InteractiveIO.ForwardChunk(AssistantChunk{Text: text})
		_ = r.InteractiveIO.ForwardFinal(AssistantMessage{Text: text})
	}
	return r.InteractiveIO.ForwardTurnEnd()
}

// finish runs once the session is over (agent exited, driver returned, or
// the session was stopped). If a turn end is still held for a correction
// turn that never ended — the agent exited on a budget limit, crashed, or
// stdin closed — it releases it as if the correction turn had ended, so the
// user gets the notice and the turn boundary.
func (r *specRepairIO) finish() {
	r.mu.Lock()
	if !r.repairing {
		r.mu.Unlock()
		return
	}
	fmt.Fprintf(os.Stderr, "[agentbox] session ended during the task-spec correction turn; releasing the held turn end\n")
	_ = r.release()
}

// NextUserMessage returns a queued correction turn if there is one — also
// when it is queued while this call is blocked — and otherwise the inner
// IO's next user message. The inner call runs in the background so a
// correction can preempt it; its result is kept for the next call. While a
// turn has reported a spec problem, the inner result is held until that
// turn ends without a correction, or until its correction turn ends, so a
// user message can't overtake the correction.
func (r *specRepairIO) NextUserMessage(ctx context.Context) (UserMessage, error) {
	select {
	case m := <-r.repairs:
		return m, nil
	default:
	}
	for {
		p := r.pending(ctx)
		select {
		case m := <-r.repairs:
			return m, nil
		case <-ctx.Done():
			return UserMessage{}, ctx.Err()
		case <-p.done:
		}
		// The inner call finished; a correction queued at the same moment,
		// or later while a spec problem holds this result, still goes first,
		// leaving this result for the next call.
		for {
			select {
			case m := <-r.repairs:
				return m, nil
			default:
			}
			r.mu.Lock()
			held, changed := r.problemSeen || r.repairing, r.changed
			r.mu.Unlock()
			if !held {
				break
			}
			select {
			case m := <-r.repairs:
				return m, nil
			case <-changed:
			case <-ctx.Done():
				return UserMessage{}, ctx.Err()
			}
		}
		r.mu.Lock()
		r.next = nil
		r.mu.Unlock()
		if p.err != nil && p.ctx.Err() != nil && ctx.Err() == nil {
			continue // started under an earlier caller's now-cancelled ctx; retry under ours
		}
		return p.msg, p.err
	}
}

// pending returns the in-flight inner NextUserMessage call, starting one
// under ctx if there is none.
func (r *specRepairIO) pending(ctx context.Context) *pendingMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.next == nil {
		p := &pendingMessage{ctx: ctx, done: make(chan struct{})}
		r.next = p
		go func() {
			p.msg, p.err = r.InteractiveIO.NextUserMessage(ctx)
			close(p.done)
		}()
	}
	return r.next
}
