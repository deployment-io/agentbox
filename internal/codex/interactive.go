package codex

import (
	"github.com/deployment-io/agentbox/internal/agent"
	"github.com/deployment-io/agentbox/internal/config"
)

// Compile-time check: the Codex driver satisfies the interactive capability.
var _ agent.InteractiveDriver = (*Driver)(nil)

// BuildInteractiveArgs launches the Codex App Server over stdio: a
// long-lived, bidirectional JSON-RPC 2.0 process (newline-delimited JSON).
// Unlike `codex exec` (one-shot batch), the app-server supports multi-turn
// sessions with streaming output — the interactive surface RunSession
// drives. Sandbox / approval policy are set per thread in the JSON-RPC
// params (see RunSession), not as CLI flags.
//
// The -c overrides mirror the batch BuildArgs: silence the GitHub update
// check and analytics/OTel traffic the agentbox proxy blocks anyway, so they
// don't add deny-log noise.
func (d *Driver) BuildInteractiveArgs(cfg *config.Config) []string {
	return []string{
		"app-server",
		"-c", "check_for_update_on_startup=false",
		"-c", "analytics.enabled=false",
		"-c", "otel.exporter=none",
		"-c", "otel.metrics_exporter=none",
	}
}

// sandboxMode is the Codex app-server sandbox for the session thread. It is
// always danger-full-access: Codex's read-only and workspace-write modes
// enforce isolation with bubblewrap, which cannot create its user namespace
// inside the agentbox container — so every command fails before it runs
// ("cannot create sandbox namespace"), which is why interactive sessions
// could converse but never investigate. The agentbox container + network
// proxy ARE the sandbox, the same model the batch `codex exec` path uses
// (--sandbox danger-full-access). Read-only intent for planning sessions is
// carried by the plan-mode prompt and the ephemeral, never-pushed clone;
// filesystem-level read-only enforcement (if wanted) is a runner concern,
// not codex's. cfg is retained for a future writable-session distinction.
func sandboxMode(_ *config.Config) string {
	return "danger-full-access"
}
