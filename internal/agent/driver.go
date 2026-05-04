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
}

// OutputParser consumes an agent's output stream and accumulates
// structured state the orchestrator reads after the subprocess exits.
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
