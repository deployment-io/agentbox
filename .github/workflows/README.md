# CI Workflows

## build.yml

Runs on every PR to `main` and every push to `main` or a `feature/**`
branch. Four jobs:

- **go** — `go build`, `go vet`, `go test -race` against Go 1.24.
- **docker** — builds the `linux/amd64` image as a smoke test (no push),
  gated on the Go job passing. Compilation happens inside the
  Dockerfile's multi-stage build; no separate `go build` step.
- **node** — `node --test` for the verify-browser script's pure logic
  (`verify-browser/lib.test.mjs`; no Playwright install).
- **docker-verify-browser** — builds `verify-browser/Dockerfile` for
  `linux/amd64` (no push), gated on the go and node jobs.

## release.yml

Runs on pushing a tag matching `v*.*.*` (e.g., `v1.0.0`, `v1.1.2`),
except `verify-browser-v*` tags — those would otherwise match too.

Gates the release on `go test -race` passing, then builds and pushes a
`linux/amd64` image to both:

- `docker.io/deploymenthq/agentbox:<version>` and `:latest`
- `ghcr.io/deployment-io/agentbox:<version>` and `:latest`

### Required secrets

Set in GitHub Settings → Secrets → Actions:

| Secret | Purpose |
|---|---|
| `DOCKERHUB_USERNAME` | Docker Hub account with push rights to `deploymenthq/agentbox` |
| `DOCKERHUB_TOKEN` | Docker Hub access token (not a password) |

GHCR authentication uses the workflow's built-in `GITHUB_TOKEN`.

## release-verify-browser.yml

Runs on pushing a tag matching `verify-browser-v*.*.*` (e.g.,
`verify-browser-v1.0.0`); the version is the tag minus `verify-browser-v`.

Gates on the same Go checks plus `node --test`, then builds
`verify-browser/Dockerfile` (context: the repository root) and pushes a
`linux/amd64` image to:

- `docker.io/deploymenthq/verify-browser:<version>` and `:latest`
- `ghcr.io/deployment-io/verify-browser:<version>` and `:latest`

Same secrets as release.yml; the Docker Hub account also needs push rights
to `deploymenthq/verify-browser`. See docs/VERIFY_BROWSER.md.

### Platform scope

Only `linux/amd64` is built and published. Multi-arch is deferred.
