# CLAUDE.md

Guidance for working on the agentbox codebase.

## Project Overview

agentbox is a Docker-based orchestrator that runs an AI coding agent
against a bind-mounted working directory. Written in Go 1.24. See
[README.md](README.md) for the user-facing description and
[docs/CONTRACT.md](docs/CONTRACT.md) for the input/output contract.

## Build & Test

Always run these after making changes to any Go file:

```bash
go build ./...
go vet ./...
go test -race ./...
```

To verify the Docker image builds:

```bash
docker build --platform linux/amd64 -t agentbox:dev .
```

The multi-stage Dockerfile compiles the Go binary inside a
`golang:1.24-bookworm` stage, so you don't need a separate
`go build` step before Docker build.

## Package Structure

```
cmd/agentbox/          # Entry point (main.go)
internal/
├── agent/             # Generic orchestrator (agent-agnostic)
│   ├── driver.go      #   Driver + OutputParser interfaces, registry
│   ├── run.go         #   Run loop, graceful shutdown, failure classification
│   ├── activity.go    #   No-activity detector (stdout watchdog)
│   └── auth.go        #   HasAuthKeyword helper (shared heuristic)
├── claude/            # Claude Code Driver (one of many possible agents)
│   ├── driver.go      #   claude.Driver + Ensure/BuildArgs/DetectVersion
│   └── parser.go      #   Stream-json OutputParser
├── config/            # Env var loading + validation
├── result/            # /result.json schema + writer
└── signals/           # SIGTERM/SIGINT → context cancellation
```

**Invariants:**
- `internal/agent/` must stay agent-agnostic. If you're tempted to
  reference Claude Code specifically from there, you're probably
  adding to the wrong package.
- Agent packages (`internal/claude/`, future `internal/aider/`, etc.)
  depend on `internal/agent/`, never the other way around.
- Shared utilities used by multiple agents live in `internal/agent/`
  (e.g., `HasAuthKeyword`).

## How to Add a New Agent

Worked example: adding hypothetical support for Aider.

**1. Create `internal/aider/driver.go`:**

```go
package aider

import (
	"context"
	"os/exec"

	"github.com/deployment-io/agentbox/internal/agent"
	"github.com/deployment-io/agentbox/internal/config"
)

const agentType = "aider"

func init() {
	agent.Register(agentType, NewDriver)
}

func NewDriver(version string) agent.Driver {
	return &Driver{version: version}
}

type Driver struct {
	version string
}

func (d *Driver) Ensure(ctx context.Context) error {
	if _, err := exec.LookPath(d.Binary()); err == nil {
		return nil
	}
	pkg := "aider-chat"
	if d.version != "" {
		pkg += "==" + d.version
	}
	cmd := exec.CommandContext(ctx, "pip", "install", "--user", pkg)
	// ... wire stdout/stderr, run, wrap error ...
	return cmd.Run()
}

func (d *Driver) Binary() string { return "aider" }

func (d *Driver) BuildArgs(cfg *config.Config) []string {
	return []string{"--yes", "--message", cfg.StepPrompt}
}

func (d *Driver) DetectVersion() string {
	out, _ := exec.Command(d.Binary(), "--version").Output()
	return strings.TrimSpace(string(out))
}

func (d *Driver) NewOutputParser() agent.OutputParser {
	return newOutputParser()
}
```

**2. Create `internal/aider/parser.go`** implementing the
`agent.OutputParser` interface for Aider's output format (diff blocks,
markdown — not stream-json).

**3. Side-effect import in `cmd/agentbox/main.go`:**

```go
import (
	_ "github.com/deployment-io/agentbox/internal/aider"
)
```

**4. Update `internal/config/config.go`** — add Aider version resolution:

```go
func agentVersionForType(agentType string) string {
	switch agentType {
	case "claude-code":
		return os.Getenv("CLAUDE_CODE_VERSION")
	case "aider":
		return os.Getenv("AIDER_VERSION")
	}
	return ""
}
```

**5. Add tests** in `internal/aider/driver_test.go` and `parser_test.go`
(mirror the patterns in `internal/claude/`).

**6. Check the Dockerfile** — Python + pip are already pre-installed
for pip-based agents, so no change needed for Aider. If a new agent
needs a runtime we don't ship (Ruby, Rust, etc.), add it in the
runtime stage.

**7. Document in README and docs/CONTRACT.md** — add the new
`AGENT_TYPE` value and any new env vars (e.g., `AIDER_VERSION`).

That's it. The Driver + OutputParser abstractions handle the rest.

## Code Conventions

- **Go 1.24**. No generics beyond stdlib-provided.
- **Functions rarely exceed 50 lines.** Decompose into private helpers
  in the same file rather than writing long functions.
- **Errors wrap with `%w`**: `fmt.Errorf("install failed: %w", err)`.
- **Small interfaces.** `Driver` has 5 methods; `OutputParser` has 2.
  Avoid growing them unless a new concern genuinely applies to every
  agent.
- **Tests use `t.Setenv`** for env manipulation — automatic cleanup,
  no race with parallel tests.
- **Package names stay short and lowercase.** `claude`, not
  `claudecode` or `claude_code_driver`.
- **No `GOWORK=off` needed** when running tests locally inside this
  repo — the parent workspace's `go.work` includes `agentbox/feature_tasks`.

## Testing Strategy

| Layer | What's tested | Where |
|---|---|---|
| Unit — config | Env parsing, credential dispatch, timeout parsing | `internal/config/*_test.go` |
| Unit — result | JSON schema, defaults, path resolution | `internal/result/*_test.go` |
| Unit — agent | Driver registry, auth-keyword heuristic, activity watchdog | `internal/agent/*_test.go` |
| Unit — claude | BuildArgs shape, stream-json parsing, auth-failure detection | `internal/claude/*_test.go` |
| Integration | End-to-end container run with real API key | Manual smoke test (README Quick Start) |
| CI | `go build` + `go vet` + `go test -race` + `docker build` | `.github/workflows/build.yml` |

Signal handling isn't unit-tested directly — it's exercised by the
manual smoke test (SIGTERM during a run).

## Dockerfile Notes

The image is multi-stage:

1. **Builder** (`golang:1.24-bookworm`): compiles
   `cmd/agentbox/main.go` as a statically linked `linux/amd64` binary.
2. **Runtime** (`debian:bookworm-slim`): contains Node 20, Python 3,
   pip, git, build-essential, and the compiled agentbox binary.
   Language runtimes are pre-installed at image build; agent-specific
   packages (Claude Code, Aider, etc.) are installed at container
   startup by the Go orchestrator's `Driver.Ensure`.

Non-root user `agent` (UID 1000) is the runtime user, with
pre-configured `NPM_CONFIG_PREFIX` and `PATH` entries for user-level
npm/pip installs.

Agent versions are pinned as build-time ARGs and exposed as ENVs:

```dockerfile
ARG CLAUDE_CODE_VERSION=2.1.117
ENV CLAUDE_CODE_VERSION=${CLAUDE_CODE_VERSION}
```

Override at build:

```bash
docker build --build-arg CLAUDE_CODE_VERSION=X.Y.Z -t agentbox:dev .
```

## Registered Agents

Maintained list of agent types consumers can set `AGENT_TYPE` to:

| Agent Type | Package | Status |
|---|---|---|
| `claude-code` | `internal/claude` | v1 |

When adding a new agent, add its row above and to the README's
"Supported Agents" section.

## Commit Message Convention

- Imperative mood ("Add", "Fix", "Refactor", not "Added" or "Adds")
- Title under 70 characters
- Body explains _why_ the change was made, not _what_ it does (the
  diff shows the what)
- Reference the phase or feature area when relevant (e.g., "Phase A
  polish: ...")
