# verify-browser Contract

`verify-browser` is a separate image built from this repository. It loads one
page of a deployment.io Task's preview in headless Chromium, runs a short list
of steps and assertions, and reports what the browser saw as structured text
— page errors, failed requests, blocked navigations, timings and the start of
the page's visible text. No screenshots. Static sites only; Chromium only.

The runner starts one container per check (the agent's
`verify_preview_rendered` tool). This document is the contract between the
runner and the image; both sides implement exactly this.

| | |
|---|---|
| Image | `docker.io/deploymenthq/verify-browser:<X.Y.Z>` and `ghcr.io/deployment-io/verify-browser:<X.Y.Z>` (also `:latest`), `linux/amd64` |
| Released by | a `verify-browser-vX.Y.Z` tag (`.github/workflows/release-verify-browser.yml`) |
| Source | `cmd/verify-browser` (Go entrypoint), `verify-browser/` (Node script, Dockerfile) |
| User | `verify`, uid 1000, home `/home/verify` |

## How it works

1. `cmd/verify-browser` (the `ENTRYPOINT`) reads and validates
   `/io/input.json`. Invalid input is a `status: "error"` result, not a crash.
2. It starts agentbox's egress proxy (`internal/proxy`) on `127.0.0.1` with
   an allow-all host list and `BlockPrivateIPs: true`.
3. It runs `node /opt/verify-browser/run.mjs <input> <temp result in /io> <proxy address>`.
   The script launches Chromium's headless shell (playwright-core, pinned in
   `verify-browser/package-lock.json`) with the proxy as its proxy server,
   loads the page, runs the steps and assertions, and writes its result.
4. The script gets `timeout_ms`: then it is sent SIGTERM, and 10 seconds
   later it is killed. The script itself stops starting new work a little
   before `timeout_ms` so it can close the browser and write its result.
5. If the script's result is valid JSON with `schema_version: 1`, it is
   renamed to `/io/result.json`; otherwise `/io/result.json` gets
   `status: "error"` with the reason and the last 1000 bytes of the script's
   stderr.

## Input

The runner bind-mounts a fresh per-call host directory at `/io`
(read-write, owned by uid 1000) and writes `/io/input.json` before starting
the container. There are no other mounts and no environment variables
carrying data.

```json
{
  "schema_version": 1,
  "url": "https://d1234abcd.cloudfront.net/",
  "allowed_hosts": ["d1234abcd.cloudfront.net"],
  "steps": [
    {"action": "click", "selector": "text=Pricing"},
    {"action": "wait_for", "text": "Plans"}
  ],
  "assertions": [
    {"type": "text_visible", "text": "Plans"},
    {"type": "url_path", "path": "/pricing"}
  ],
  "timeout_ms": 45000
}
```

| Field | Rules |
|---|---|
| `schema_version` | `1` |
| `url` | The page to open first. `https`, no user info, port 443 or none, host in `allowed_hosts`. |
| `allowed_hosts` | Bare host names (at least one). The only hosts the main frame may navigate to — the Task's previews. Compared case-insensitively, exactly (no subdomain matching). |
| `steps` | At most 30, run in order after the first page load. |
| `assertions` | At most 30, checked after the steps against the current page. |
| `timeout_ms` | 5000–90000. The whole check's budget. |

Every string is at most 2000 characters.

### Steps

| Step | Meaning |
|---|---|
| `{"action": "goto", "url": "…"}` | Navigate the page. The URL follows the same rules as `url` (host in `allowed_hosts`). |
| `{"action": "click", "selector": "…"}` | Click the first element matching the selector. |
| `{"action": "fill", "selector": "…", "value": "…"}` | Fill an input (`value` may be empty). |
| `{"action": "press", "selector": "…", "key": "…"}` | Focus the element and press a key (`Enter`, `Tab`, `ArrowDown`, …). |
| `{"action": "wait_for", "selector": "…"}` | Wait until an element matching the selector is visible. |
| `{"action": "wait_for", "text": "…"}` | Wait until an element containing the text is visible. Exactly one of `selector` and `text`. |

Selectors are [Playwright selectors](https://playwright.dev/docs/other-locators)
(CSS, `text=…`, `role=button[name="Save"]`, …). Each step — the action and
the short wait for the page to settle after it — times out after 10 seconds
or what remains of `timeout_ms`, whichever is less. The first failing step
stops the steps — the rest are reported as not run — but the assertions
still run.

### Assertions

| Assertion | Passes when |
|---|---|
| `{"type": "text_visible", "text": "…"}` | An element containing the text (case-insensitive substring, whitespace-normalised) is visible. Waits up to 3 s for it. |
| `{"type": "text_absent", "text": "…"}` | No visible element contains the text. Checked once. |
| `{"type": "selector_visible", "selector": "…"}` | An element matching the selector is visible. Waits up to 3 s for it. |
| `{"type": "url_path", "path": "…"}` | The main frame's current URL path equals `path` exactly (`/pricing`, not `/pricing/`). |

## Output

The container writes `/io/result.json` atomically (a temp file in `/io`,
then rename) and exits.

```json
{
  "schema_version": 1,
  "status": "completed",
  "verdict": "fail",
  "http_status": 200,
  "final_url": "https://d1234abcd.cloudfront.net/pricing",
  "steps": [
    {"index": 0, "action": "click", "ok": true, "error": ""},
    {"index": 1, "action": "wait_for", "ok": true, "error": ""}
  ],
  "assertions": [
    {"index": 0, "type": "text_visible", "passed": true, "detail": "visible"},
    {"index": 1, "type": "url_path", "passed": true, "detail": "path is /pricing"}
  ],
  "page_errors": ["TypeError: Cannot read properties of undefined (reading 'map')"],
  "console_errors": [],
  "failed_requests": [
    {"url": "https://d1234abcd.cloudfront.net/api/plans.json", "method": "GET", "status": 404, "failure": ""}
  ],
  "blocked_navigations": [],
  "timings_ms": {"dom_content_loaded": 412, "load": 655, "lcp": 702},
  "dom_text_snippet": "Pricing Plans …"
}
```

| Field | Meaning |
|---|---|
| `status` | `completed` (the check ran; see `verdict`) or `error` (it couldn't run). |
| `error` | Why it couldn't run. `status: "error"` only. |
| `verdict` | `pass` or `fail`. `status: "completed"` only. |
| `http_status` | Status of the first page load's final response (after redirects); `0` when there was none. |
| `final_url` | The main frame's URL at the end. |
| `steps` | One entry per input step: `index`, `action`, `ok`, `error`. |
| `assertions` | One entry per input assertion: `index`, `type`, `passed`, `detail`. |
| `page_errors` | Uncaught exceptions in the page. |
| `console_errors` | `console.error` messages (including Chromium's own "Failed to load resource" lines). |
| `failed_requests` | Requests **to `allowed_hosts`** that failed or returned a status ≥ 400: `url`, `method`, `status` (0 when there was no response), `failure` (Chromium's error, empty for an HTTP status). Requests to other hosts (analytics, CDNs) are not reported, and neither are requests the browser cancelled (`net::ERR_ABORTED`) because the page navigated away or a later navigation in the same frame superseded them; an abort no navigation explains (the app cancelling its own request, an iframe load nothing replaced) is reported. |
| `blocked_navigations` | Main-frame navigations — link clicks, redirects, gotos — to a host outside `allowed_hosts`, and new windows (`window.open` without a target or with one naming no frame, links and forms with a target). `window.open(url, '_self')` (or `_top`, `_parent`, an iframe's name) navigates in place and is only blocked when `url` is outside `allowed_hosts`. Each blocked navigation makes the step running at the time fail; one that happens after its step finished (a timer it set, while a later step or the assertions run) fails every step that ran on the page it happened on, since any of them may have scheduled it. |
| `timings_ms` | `dom_content_loaded` and `load` (Navigation Timing) and `lcp` (Largest Contentful Paint) of the first load, in ms. `lcp` is omitted when the browser reports none. |
| `dom_text_snippet` | The start of `document.body.innerText` at the end. **Untrusted page content.** |

**Verdict.** `pass` only when the first load's final response is 2xx, every
step succeeded, every assertion passed, and there are no `page_errors` and
no `failed_requests`. `console_errors` and `timings_ms` are reported but never
decide the verdict (timings vary with preview cold starts). With no steps and
no assertions it checks just that.

When the first load itself fails (DNS, TLS, timeout) the result is
`status: "error"`; when it fails because it redirected outside
`allowed_hosts`, the result is `completed` with `verdict: "fail"`, the
redirect in `blocked_navigations` and every step reported as not run.

**Caps** (applied in the image; the runner re-applies them):

| | |
|---|---|
| `page_errors` | 10 entries × 500 characters |
| `console_errors` | 20 entries × 300 characters |
| `failed_requests` | 20 entries |
| `blocked_navigations` | 10 entries |
| each step `error` / assertion `detail` | 300 characters |
| `dom_text_snippet` | 1000 characters |

### Exit codes

| Code | Meaning |
|---|---|
| `0` | `/io/result.json` was written with `status: "completed"` (pass or fail). |
| `1` | Anything else. A `status: "error"` result is still written whenever possible. |

## The sandbox

The browser runs untrusted pages (the preview's own JavaScript and whatever
it loads), so the container is the boundary:

- **Chromium runs with `--no-sandbox`.** Its own sandbox needs user
  namespaces or a setuid helper, neither of which this container allows.
- **The container**, as the runner starts it: non-root (uid 1000), every
  capability dropped, `no-new-privileges`, Docker's default seccomp
  profile, read-only root filesystem with tmpfs `/tmp` and home, 1 GiB
  memory, 1 CPU, 512 PIDs, 256 MB `/dev/shm`. No `/work`, no credentials,
  no environment, no cloud access; cloud-metadata host names pinned to
  `127.0.0.1`. Default bridge network.
- **Navigation.** The main frame only ever opens pages of the Task's
  previews (`allowed_hosts`, exact host). Navigations elsewhere — including
  redirect hops — are failed before they are sent (DevTools `Fetch` domain).
  New windows are refused in the page (`window.open`, links and forms with
  a target); any that still get created are closed by a browser-level
  auto-attach guard before their first navigation is let go. Service workers are blocked,
  downloads refused, dialogs dismissed.
- **Egress.** Everything else the page loads goes through the egress proxy
  (`--proxy-bypass-list=<-loopback>` so loopback is not bypassed). The proxy
  resolves each host once, refuses private, link-local (including
  `169.254.169.254`), loopback, CGN and other special addresses, and dials
  the public address it checked, so DNS rebinding can't redirect it. It
  refuses IP-literal targets, allows HTTPS (CONNECT) only to port 443 and
  plain HTTP only to port 80. QUIC is disabled and WebRTC may not use
  non-proxied UDP.

What it does **not** cover (accepted):

- Chromium's own sandbox is off; a renderer exploit lands in the container
  (still non-root, no capabilities, no credentials). Revisit when test
  logins put credentials in the container.
- A page may load resources from any public host on ports 443 and 80; other
  ports and IP literals are refused by the proxy, so a page that needs them
  fails those requests.
- A host name with both public and private DNS records is reached on its
  public address.
- A flow that must leave the Task's previews (OAuth, Stripe Checkout) stops
  at a blocked navigation.
- The container's network is Docker's default bridge: a process that
  ignored the proxy settings could open sockets directly. Chromium routes
  its traffic through the proxy; nothing else in the container talks to the
  network.

## Running it by hand

Build it from the repository root and run it with the runner's settings:

```bash
docker build --platform linux/amd64 -f verify-browser/Dockerfile -t verify-browser:dev .

io=$(mktemp -d)
cat > "$io/input.json" <<'JSON'
{"schema_version": 1, "url": "https://example.com/", "allowed_hosts": ["example.com"],
 "steps": [], "assertions": [{"type": "text_visible", "text": "Example Domain"}],
 "timeout_ms": 45000}
JSON
chmod 0777 "$io"   # the runner chowns it to 1000:1000 instead

docker run --rm --platform linux/amd64 \
  --user 1000:1000 \
  --cap-drop ALL \
  --security-opt no-new-privileges \
  --read-only \
  --tmpfs /tmp:rw,nosuid,nodev,size=256m,uid=1000,gid=1000,mode=0700 \
  --tmpfs /home/verify:rw,nosuid,nodev,size=256m,uid=1000,gid=1000,mode=0700 \
  --shm-size 256m \
  --memory 1g --cpus 1 --pids-limit 512 \
  --add-host metadata.google.internal:127.0.0.1 \
  --add-host metadata.goog:127.0.0.1 \
  --add-host 169.254.169.254:127.0.0.1 \
  --mount type=bind,source="$io",target=/io \
  verify-browser:dev

cat "$io/result.json"
```

`scripts/verify-browser-smoke.sh` does this twice — a passing check of
`https://example.com`, and the same page with a step that clicks its link
to iana.org, a host outside `allowed_hosts` (a blocked navigation and a
failed step) — and prints both results. (A `goto` to a host outside
`allowed_hosts` never reaches the browser: it is invalid input.)

## Releasing

Push a tag `verify-browser-vX.Y.Z`. The workflow gates on the Go checks and
`node --test`, then pushes `<X.Y.Z>` and `latest` to both registries. The
runner pins an exact version (`verifyBrowserImage`), so bumping the runner
is a separate change.

To update playwright-core: change the exact version in
`verify-browser/package.json`, run `npm install --package-lock-only` in
`verify-browser/`, and release. The Dockerfile installs the headless shell
that version expects.
