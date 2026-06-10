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

// sandboxMode maps the session's read-only flag to the Codex app-server
// sandbox setting. Read-only investigation sessions (the Assistant default)
// get "readOnly"; others get workspace-write.
func sandboxMode(cfg *config.Config) string {
	if cfg.ReadOnly {
		return "read-only"
	}
	return "workspace-write"
}
