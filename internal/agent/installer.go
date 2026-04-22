package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/deployment-io/agentbox/internal/config"
)

// Installer encapsulates the per-agent concerns: how to install it, what
// binary to exec, how to build its command line, and how to detect the
// installed version for result.json attribution.
type Installer interface {
	Ensure(ctx context.Context) error
	Binary() string
	BuildArgs(cfg *config.Config) []string
	DetectVersion() string
}

// InstallerFor returns the registered Installer for an agent type. v1
// supports only "claude-code"; v2+ adds more via registration here.
func InstallerFor(agentType, version string) (Installer, error) {
	switch agentType {
	case "claude-code":
		return &claudeCodeInstaller{version: version}, nil
	}
	return nil, fmt.Errorf("unsupported AGENT_TYPE %q", agentType)
}

// claudeCodeInstaller installs Claude Code via npm install -g.
type claudeCodeInstaller struct {
	version string
}

func (c *claudeCodeInstaller) Ensure(ctx context.Context) error {
	if _, err := exec.LookPath(c.Binary()); err == nil {
		return nil
	}
	pkg := "@anthropic-ai/claude-code"
	if c.version != "" {
		pkg += "@" + c.version
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

func (c *claudeCodeInstaller) Binary() string {
	return "claude"
}

func (c *claudeCodeInstaller) BuildArgs(cfg *config.Config) []string {
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

func (c *claudeCodeInstaller) DetectVersion() string {
	out, err := exec.Command(c.Binary(), "--version").Output()
	if err != nil {
		return ""
	}
	// `claude --version` prints "X.Y.Z (Claude Code)"; keep just the semver.
	if fields := strings.Fields(string(out)); len(fields) > 0 {
		return fields[0]
	}
	return strings.TrimSpace(string(out))
}
