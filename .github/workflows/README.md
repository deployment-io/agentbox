# CI Workflows

## build.yml

Runs on every PR to `main` and every push to `main` or a `feature/**`
branch. Two jobs:

- **go** — `go build`, `go vet`, `go test -race` against Go 1.24.
- **docker** — builds the `linux/amd64` image as a smoke test (no push),
  gated on the Go job passing. Compilation happens inside the
  Dockerfile's multi-stage build; no separate `go build` step.

## release.yml

Runs on pushing a tag matching `v*.*.*` (e.g., `v1.0.0`, `v1.1.2`).

Gates the release on `go test -race` passing, then builds and pushes a
`linux/amd64` image to both:

- `docker.io/deploymentio/agentbox:<version>` and `:latest`
- `ghcr.io/deployment-io/agentbox:<version>` and `:latest`

### Required secrets

Set in GitHub Settings → Secrets → Actions:

| Secret | Purpose |
|---|---|
| `DOCKERHUB_USERNAME` | Docker Hub account with push rights to `deploymentio/agentbox` |
| `DOCKERHUB_TOKEN` | Docker Hub access token (not a password) |

GHCR authentication uses the workflow's built-in `GITHUB_TOKEN`.

### Platform scope

Only `linux/amd64` is built and published. Multi-arch is deferred.
