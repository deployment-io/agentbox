// Package claude provides the Driver implementation for Anthropic's
// Claude Code CLI. Registers itself with the agent package at init time;
// consumers side-effect-import this package to make "claude-code"
// resolvable via agent.DriverFor.
package claude

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/deployment-io/agentbox/internal/agent"
	"github.com/deployment-io/agentbox/internal/config"
)

const agentType = "claude-code"

func init() {
	agent.Register(agentType, NewDriver)
}

// NewDriver constructs a Driver for Claude Code at the given pinned version.
func NewDriver(version string) agent.Driver {
	return &Driver{version: version}
}

// Driver installs and runs Claude Code via npm.
type Driver struct {
	version string
}

func (d *Driver) Ensure(ctx context.Context) error {
	if _, err := exec.LookPath(d.Binary()); err == nil {
		return nil
	}
	pkg := "@anthropic-ai/claude-code"
	if d.version != "" {
		pkg += "@" + d.version
	}
	fmt.Fprintf(os.Stderr, "[agentbox] installing %s\n", pkg)
	cmd := exec.CommandContext(ctx, "npm", "install", "-g", pkg)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("npm install -g %s failed: %w", pkg, err)
	}
	return nil
}

func (d *Driver) Binary() string {
	return "claude"
}

func (d *Driver) BuildArgs(cfg *config.Config) []string {
	// Claude Code rejects --output-format=stream-json + -p without --verbose.
	args := []string{
		"-p", cfg.StepPrompt,
		"--output-format", "stream-json",
		"--verbose",
		"--dangerously-skip-permissions",
	}
	if cfg.MaxTurns != "" {
		args = append(args, "--max-turns", cfg.MaxTurns)
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	return args
}

func (d *Driver) DetectVersion() string {
	out, err := exec.Command(d.Binary(), "--version").Output()
	if err != nil {
		return ""
	}
	// `claude --version` prints "X.Y.Z (Claude Code)"; keep just the semver.
	if fields := strings.Fields(string(out)); len(fields) > 0 {
		return fields[0]
	}
	return strings.TrimSpace(string(out))
}

func (d *Driver) NewOutputParser() agent.OutputParser {
	return newStreamParser()
}
