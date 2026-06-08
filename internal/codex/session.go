package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/deployment-io/agentbox/internal/agent"
	"github.com/deployment-io/agentbox/internal/config"
	"github.com/deployment-io/agentbox/internal/spec"
)

// RunSession drives a Codex App Server session over JSON-RPC 2.0:
//
//	initialize → initialized → thread/start → (turn/start → stream)*
//
// Per turn it sends the user's message and streams the agent's reply back
// via sess.IO: item/agentMessage/delta → ForwardChunk, the completed agent
// message + turn/completed → ForwardFinal + any task-spec. The session is
// autonomous and read-only (approvalPolicy "never" + a read-only sandbox),
// so the server never sends approval round-trips. Returns nil on a clean end
// (user input exhausted or stdout EOF) and an error on a protocol failure.
//
// The exact method names / param shapes follow the App Server protocol docs
// (developers.openai.com/codex/app-server); the interactive harness is used
// to confirm them against the running CLI and adjust if a field differs.
func (d *Driver) RunSession(ctx context.Context, cfg *config.Config, sess *agent.InteractiveSession) error {
	hbCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()
	go agent.RunHeartbeat(hbCtx, agent.HeartbeatInterval, sess.IO, func() agent.SessionState {
		return agent.SessionState{}
	})

	defer sess.Stdin.Close()
	enc := json.NewEncoder(sess.Stdin)

	r := newRPCReader(sess.Stdout, sess.Log)
	defer r.close()

	// 1. Handshake.
	if err := enc.Encode(rpcRequest{ID: 0, Method: "initialize", Params: initializeParams{
		ClientInfo: clientInfo{Name: "agentbox", Title: "agentbox", Version: "1"},
	}}); err != nil {
		return fmt.Errorf("codex initialize: %w", err)
	}
	if err := r.awaitResponse(0); err != nil {
		return fmt.Errorf("codex initialize: %w", err)
	}
	if err := enc.Encode(rpcNotification{Method: "initialized", Params: emptyParams{}}); err != nil {
		return fmt.Errorf("codex initialized: %w", err)
	}

	// 2. Start a thread — autonomous, read-only, no approval round-trips.
	if err := enc.Encode(rpcRequest{ID: 1, Method: "thread/start", Params: threadStartParams{
		Cwd:            cfg.WorkDir,
		Model:          cfg.Model,
		ApprovalPolicy: "never",
		Sandbox:        sandboxMode(cfg),
	}}); err != nil {
		return fmt.Errorf("codex thread/start: %w", err)
	}
	threadID, err := r.awaitThreadID(1)
	if err != nil {
		return fmt.Errorf("codex thread/start: %w", err)
	}

	// 3. Turn loop.
	nextID := 2
	first := true
	for {
		msg, err := sess.IO.NextUserMessage(ctx)
		if err != nil {
			return nil // ctx cancelled or end-of-input: clean session end
		}
		text := msg.Text
		if first && cfg.AppendSystemPrompt != "" {
			// The app-server has no separate system-prompt channel here, so
			// the plan-mode instructions ride on the first user turn.
			text = cfg.AppendSystemPrompt + "\n\n" + text
		}
		first = false

		if err := enc.Encode(rpcRequest{ID: nextID, Method: "turn/start", Params: turnStartParams{
			ThreadID: threadID,
			Input:    []inputItem{{Type: "text", Text: text}},
		}}); err != nil {
			return fmt.Errorf("codex turn/start: %w", err)
		}
		nextID++
		if err := r.runTurn(sess.IO); err != nil {
			return err
		}
	}
}

// --- JSON-RPC wire types ---

// rpcRequest / rpcNotification match the Codex app-server schema, which
// requires only {id, method, params} (and {method, params}) — notably NO
// "jsonrpc" field; sending one risks a strict-deserialize rejection.
type rpcRequest struct {
	ID     int    `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

type rpcNotification struct {
	Method string `json:"method"`
	Params any    `json:"params"`
}

type emptyParams struct{}

type initializeParams struct {
	ClientInfo clientInfo `json:"clientInfo"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Version string `json:"version"`
}

type threadStartParams struct {
	Cwd            string `json:"cwd"`
	Model          string `json:"model,omitempty"`
	ApprovalPolicy string `json:"approvalPolicy,omitempty"`
	Sandbox        string `json:"sandbox,omitempty"`
}

type turnStartParams struct {
	ThreadID string      `json:"threadId"`
	Input    []inputItem `json:"input"`
}

type inputItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// rpcMessage is the union of an inbound response (ID + Result/Error) and a
// notification (Method + Params).
type rpcMessage struct {
	ID     *int            `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Params json.RawMessage `json:"params"`
	Error  *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// --- reader ---

type rpcReader struct {
	sc  *bufio.Scanner
	raw io.WriteCloser // unfiltered stream debug log; may be nil
	log io.Writer      // human-readable container log
}

func newRPCReader(stdout io.Reader, logSink io.Writer) *rpcReader {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	return &rpcReader{sc: sc, raw: openRawStreamLog(), log: logSink}
}

func (r *rpcReader) close() {
	if r.raw != nil {
		_ = r.raw.Close()
	}
}

// next reads and decodes the next JSON-RPC message, skipping non-JSON or
// malformed lines. Returns io.EOF when the stream ends.
func (r *rpcReader) next() (*rpcMessage, error) {
	for r.sc.Scan() {
		line := r.sc.Bytes()
		if r.raw != nil {
			_, _ = r.raw.Write(line)
			_, _ = r.raw.Write([]byte{'\n'})
		}
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			continue
		}
		var m rpcMessage
		if err := json.Unmarshal(trimmed, &m); err != nil {
			continue
		}
		return &m, nil
	}
	if err := r.sc.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

// awaitResponse reads until the response with the given id, ignoring
// notifications and other ids in the meantime.
func (r *rpcReader) awaitResponse(id int) error {
	for {
		m, err := r.next()
		if err != nil {
			return err
		}
		if m.ID != nil && *m.ID == id {
			if m.Error != nil {
				return fmt.Errorf("rpc error %d: %s", m.Error.Code, m.Error.Message)
			}
			return nil
		}
	}
}

// awaitThreadID reads the thread/start response and extracts the thread id.
func (r *rpcReader) awaitThreadID(id int) (string, error) {
	for {
		m, err := r.next()
		if err != nil {
			return "", err
		}
		if m.ID == nil || *m.ID != id {
			continue
		}
		if m.Error != nil {
			return "", fmt.Errorf("rpc error %d: %s", m.Error.Code, m.Error.Message)
		}
		var res struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		if err := json.Unmarshal(m.Result, &res); err != nil {
			return "", fmt.Errorf("parse thread/start result: %w", err)
		}
		if res.Thread.ID == "" {
			return "", fmt.Errorf("thread/start returned no thread id")
		}
		return res.Thread.ID, nil
	}
}

// runTurn reads notifications until the turn completes, forwarding assistant
// text and finalizing the message + spec at turn end.
func (r *rpcReader) runTurn(out agent.InteractiveSink) error {
	// streamed tracks whether deltas were forwarded for the in-progress
	// agent message, so a completed message isn't re-sent as a duplicate
	// chunk. Reset after each agent message is finalized.
	streamed := false
	for {
		m, err := r.next()
		if err != nil {
			return err
		}
		switch m.Method {
		case "item/agentMessage/delta":
			var p struct {
				Delta string `json:"delta"`
			}
			if json.Unmarshal(m.Params, &p) == nil && p.Delta != "" {
				streamed = true
				_ = out.ForwardChunk(agent.AssistantChunk{Text: p.Delta})
			}
		case "item/completed":
			// Codex emits multiple agentMessage items per turn (progress
			// narration plus the final answer). Finalize each as its own
			// message so the chat renders distinct bubbles, rather than
			// collapsing the whole turn into one.
			var p struct {
				Item struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"item"`
			}
			if json.Unmarshal(m.Params, &p) == nil && p.Item.Type == "agentMessage" {
				r.finalizeMessage(out, p.Item.Text, streamed)
				streamed = false
			}
		case "turn/completed":
			return nil
		case "turn/failed":
			fmt.Fprintln(r.log, "[codex] turn failed")
			return nil
		case "error":
			var p struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(m.Params, &p)
			return fmt.Errorf("codex app-server error: %s", p.Message)
		}
		// Responses (m.ID != nil) and other notifications are ignored.
	}
}

// finalizeMessage forwards one completed agent message: any task-spec it
// carries, then the user-visible text — as a chunk if it wasn't already
// streamed via deltas — and the final.
func (r *rpcReader) finalizeMessage(out agent.InteractiveSink, text string, streamed bool) {
	if s, ok := spec.Extract(text); ok {
		_ = out.ForwardSpecUpdate(s)
	}
	display := spec.Strip(text)
	if display == "" {
		return
	}
	if !streamed {
		_ = out.ForwardChunk(agent.AssistantChunk{Text: display})
	}
	_ = out.ForwardFinal(agent.AssistantMessage{Text: display})
}
