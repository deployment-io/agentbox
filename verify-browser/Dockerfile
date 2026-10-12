# verify-browser: loads a Task's preview in headless Chromium and reports what
# the browser saw. Contract and run instructions: docs/VERIFY_BROWSER.md.
#
# Build from the repository root (the Go entrypoint uses internal/ packages):
#
#   docker build --platform linux/amd64 -f verify-browser/Dockerfile -t verify-browser:dev .

# ---- Stage 1: Go entrypoint ----
FROM golang:1.25-bookworm AS builder

WORKDIR /src
# Only what cmd/verify-browser needs, so agentbox changes elsewhere don't
# invalidate this layer.
COPY go.mod ./
COPY internal/proxy ./internal/proxy
COPY internal/signals ./internal/signals
COPY cmd/verify-browser ./cmd/verify-browser
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/verify-browser ./cmd/verify-browser


# ---- Stage 2: Runtime ----
FROM node:22-bookworm-slim

# Browsers live outside the user's home, which the runner covers with a tmpfs.
ENV PLAYWRIGHT_BROWSERS_PATH=/opt/ms-playwright

WORKDIR /opt/verify-browser
COPY verify-browser/package.json verify-browser/package-lock.json ./

# playwright-core at the exact version package-lock.json pins, then Chromium's
# headless shell (not the full browser) and the system libraries it needs.
# npm, corepack and yarn are removed afterwards: nothing runs them at runtime.
RUN npm ci --omit=dev --ignore-scripts \
    && node node_modules/playwright-core/cli.js install --with-deps --only-shell chromium \
    && rm -rf /var/lib/apt/lists/* /var/cache/apt/* /root/.npm /root/.cache /tmp/* \
    && rm -rf /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/corepack \
       /usr/local/bin/npm /usr/local/bin/npx /usr/local/bin/corepack \
       /usr/local/bin/yarn /usr/local/bin/yarnpkg /opt/yarn-*

COPY verify-browser/lib.mjs verify-browser/run.mjs ./
COPY --from=builder /out/verify-browser /usr/local/bin/verify-browser

# Non-root uid 1000 that owns nothing but its home. The base image's `node`
# user also has uid 1000; it is replaced so the name says what it is for.
RUN userdel -r node \
    && useradd -m -u 1000 -s /usr/sbin/nologin verify

LABEL org.opencontainers.image.title="verify-browser"
LABEL org.opencontainers.image.description="Headless Chromium check of a deployment.io Task preview"
LABEL org.opencontainers.image.source="https://github.com/deployment-io/agentbox"
LABEL org.opencontainers.image.licenses="Apache-2.0"

USER 1000:1000
WORKDIR /home/verify

ENTRYPOINT ["/usr/local/bin/verify-browser"]
