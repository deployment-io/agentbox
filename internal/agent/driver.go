// Package agent is the generic orchestrator. It defines the Driver and
// OutputParser interfaces that agent-specific packages (e.g., claude)
// implement, plus the Run loop that wires everything together.
package agent

import (
	"context"
	"fmt"
	"io"

	"github.com/deployment-io/agentbox/internal/config"
	"github.com/deployment-io/agentbox/internal/result"
)

// Driver encapsulates per-agent concerns: install, exec, version, output,
// and the network-allowlist for the built-in proxy. Implementations live
// in agent-specific packages (claude, aider, ...).
type Driver interface {
	Ensure(ctx context.Context) error
	Binary() string
	BuildArgs(cfg *config.Config) []string
	DetectVersion() string
	NewOutputParser() OutputParser
	// AllowedHosts returns the hostnames this agent legitimately needs
	// outbound HTTPS access to (e.g., its API endpoint, package registry
	// for install). The agentbox proxy unions this with the user-supplied
	// ADDITIONAL_ALLOWED_HOSTS env var to form the final allowlist.
	// Empty slice == agent doesn't need any outbound access.
	AllowedHosts() []string
	// NewLogFormatter wraps sink with a writer that translates the
	// agent's raw stdout into a compact human-readable form (one short
	// summary line per logical event). The returned WriteCloser is the
	// stdout sink used by the Run loop; Close flushes any pending
	// partial line and releases any driver-owned debug resources. A
	// driver that doesn't want to transform should return a writer that
	// passes through verbatim.
	//
	// Concern is per-agent because each agent prints in its own format
	// (Claude Code: stream-json; Aider: diff+markdown). Output suitable
	// for programmatic parsing is rarely suitable for human log viewing.
	NewLogFormatter(sink io.Writer) io.WriteCloser
}

// OutputParser consumes an agent's output stream and accumulates
// structured state the orchestrator reads.
//
// Concurrency contract:
//
//   - Consume(r) is called from one goroutine — the Run loop's reader.
//     Implementations don't need to guard against concurrent Consume
//     calls.
//
//   - State() MUST be safe to call concurrently with Consume(). The
//     progress writer (internal/progress) snapshots State() on its own
//     ticker while the agent's output is still streaming through
//     Consume(); without synchronization the snapshot could observe a
//     torn read (e.g., turns counter updated but token usage not yet
//     reflecting the same event) or a data race on a map/slice mid-
//     mutation. The progress writer was added in Phase 5.5b; the
//     pre-Phase-5.5b assumption that State() was only called after
//     Consume returned no longer holds.
//
//     Typical implementation: a sync.Mutex guarding every mutation
//     inside Consume's event-handling path, plus State() taking the
//     lock and returning a copy of the accumulated state. See
//     internal/claude/parser.go's streamParser for the reference
//     pattern.
type OutputParser interface {
	Consume(r io.Reader)
	State() ParsedState
}

// ParsedState is what orchestrator reads out of a parser after the run.
type ParsedState struct {
	ChangesSummary string
	FilesChanged   []string
	TokenUsage     result.TokenUsage
	Turns          int
	IsError        bool
	IsAuthFailure  bool

	// Model is the actual model identifier the agent reported using
	// (when the agent exposes one). Generic across agents — each parser
	// populates from whatever its protocol surfaces, or leaves empty.
	// For Claude Code, captured from the stream-json system.init event
	// (with assistant-message.model as a fallback).
	Model string
}

// DriverFactory builds a Driver for the given pinned version.
type DriverFactory func(version string) Driver

var registry = map[string]DriverFactory{}

// Register associates an agent type with its Driver factory. Typically
// called from a driver package's init(); panics on duplicate registration
// so misconfigurations surface at startup.
func Register(agentType string, factory DriverFactory) {
	if _, exists := registry[agentType]; exists {
		panic(fmt.Sprintf("agent: duplicate Register for type %q", agentType))
	}
	registry[agentType] = factory
}

// DriverFor returns the Driver registered for the given agent type.
func DriverFor(agentType, version string) (Driver, error) {
	factory, ok := registry[agentType]
	if !ok {
		return nil, fmt.Errorf("unsupported AGENT_TYPE %q", agentType)
	}
	return factory(version), nil
}
