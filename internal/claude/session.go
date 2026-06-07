package claude

import (
	"bufio"
	"context"
	"io"

	"github.com/deployment-io/agentbox/internal/agent"
	"github.com/deployment-io/agentbox/internal/config"
)

// Compile-time check: the Claude driver satisfies the interactive capability.
var _ agent.InteractiveDriver = (*Driver)(nil)

// RunSession drives a Claude Code stream-json session: it pumps user turns
// to stdin (one self-contained stream-json envelope each, via PumpTextStdin)
// and reads the agent's stdout line-by-line, teeing the raw bytes to the
// human log formatter while feeding each complete line to the chunk
// forwarder (which streams assistant text, finalizes each turn's message,
// and extracts any task-spec). It returns when stdout reaches EOF (the agent
// exited) or on a read error.
func (d *Driver) RunSession(ctx context.Context, cfg *config.Config, sess *agent.InteractiveSession) error {
	humanFmt := newHumanLogFormatter(sess.Log, openRawStreamLog())
	defer humanFmt.Close()

	// Liveness heartbeat, independent of agent output. Stats are minimal in
	// v1 — the forwarded chunks are the primary activity signal.
	hbCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()
	go agent.RunHeartbeat(hbCtx, agent.HeartbeatInterval, sess.IO, func() agent.SessionState {
		return agent.SessionState{}
	})

	// Pump user turns to stdin; PumpTextStdin owns and closes stdin.
	go agent.PumpTextStdin(ctx, sess.IO, d.encodeUserMessage, sess.Stdin)

	// Read agent stdout: tee raw bytes to the human log formatter while
	// scanning complete lines for the structured forwarder.
	fwd := newChunkForwarder(sess.IO)
	scanner := bufio.NewScanner(io.TeeReader(sess.Stdout, humanFmt))
	const maxLine = 10 * 1024 * 1024 // tool inputs / result summaries can be large
	scanner.Buffer(make([]byte, 0, 64*1024), maxLine)
	for scanner.Scan() {
		fwd.handleLine(scanner.Bytes())
	}
	return scanner.Err()
}
