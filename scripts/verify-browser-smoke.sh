#!/usr/bin/env bash
# Builds the verify-browser image and runs it twice with the runner's
# container settings (docs/VERIFY_BROWSER.md):
#   1. https://example.com with a text_visible "for use in documentation examples" assertion —
#      expect status completed, verdict pass.
#   2. the same page, clicking its link to iana.org (outside allowed_hosts) —
#      expect a blocked navigation, a failed step and verdict fail.
#   3. https://httpbin.org/forms/post with read_only, submitting the form —
#      expect the POST in blocked_requests, every step ok, and final_url still
#      the form (httpbin's /post response is never reached).
#   4. the same page and steps without read_only — expect blocked_requests []
#      and final_url httpbin's /post.
#   5. https://httpbin.org/forms/post, going to /html and then back — expect
#      verdict pass with url_path /forms/post.
#   6. the same page with only a back step — expect steps[0].ok false with the
#      no-earlier-page error.
# Cases 3 to 6 are asserted (they need jq); the script exits non-zero when
# either does not hold. Needs Docker and network access. Run from anywhere in
# the repository.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
image="${VERIFY_BROWSER_SMOKE_IMAGE:-verify-browser:smoke}"

docker build --platform linux/amd64 -f "$repo_root/verify-browser/Dockerfile" -t "$image" "$repo_root"

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

# run_check NAME INPUT_JSON — runs one check and prints its result.
run_check() {
  local name="$1" input="$2" io="$workdir/$1"
  mkdir -p "$io"
  printf '%s\n' "$input" > "$io/input.json"
  # The runner chowns /io to 1000:1000; a world-writable dir works without root.
  chmod 0777 "$io"
  chmod 0644 "$io/input.json"
  local code=0
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
    --mount "type=bind,source=$io,target=/io" \
    "$image" > "$io/output.log" 2>&1 || code=$?
  echo "=== $name (exit $code) ==="
  if [ -f "$io/result.json" ]; then
    if command -v jq >/dev/null 2>&1; then jq . "$io/result.json"; else cat "$io/result.json"; echo; fi
  else
    echo "no result.json; container output:"
    tail -c 2000 "$io/output.log"
  fi
}

run_check pass '{
  "schema_version": 1,
  "url": "https://example.com/",
  "allowed_hosts": ["example.com"],
  "steps": [],
  "assertions": [{"type": "text_visible", "text": "for use in documentation examples"}],
  "timeout_ms": 45000
}'

run_check blocked '{
  "schema_version": 1,
  "url": "https://example.com/",
  "allowed_hosts": ["example.com"],
  "steps": [{"action": "click", "selector": "a[href*=\"iana.org\"]"}],
  "assertions": [{"type": "text_visible", "text": "for use in documentation examples"}],
  "timeout_ms": 45000
}'

echo "Expected: 'pass' → verdict pass; 'blocked' → verdict fail, blocked_navigations has the iana.org URL, steps[0].ok false."

# form_input READ_ONLY — fills httpbin's order form and submits it.
form_input() {
  cat <<JSON
{
  "schema_version": 1,
  "url": "https://httpbin.org/forms/post",
  "allowed_hosts": ["httpbin.org"],
  "read_only": $1,
  "steps": [
    {"action": "fill", "selector": "input[name=custname]", "value": "verify-browser smoke"},
    {"action": "click", "selector": "text=Submit order"}
  ],
  "assertions": [],
  "timeout_ms": 45000
}
JSON
}

run_check read_only "$(form_input true)"
run_check read_write "$(form_input false)"

# back_input STEPS — httpbin's form page, the given steps, and a url_path
# assertion on the form.
back_input() {
  cat <<JSON
{
  "schema_version": 1,
  "url": "https://httpbin.org/forms/post",
  "allowed_hosts": ["httpbin.org"],
  "steps": $1,
  "assertions": [{"type": "url_path", "path": "/forms/post"}],
  "timeout_ms": 45000
}
JSON
}

run_check back "$(back_input '[{"action": "goto", "url": "https://httpbin.org/html"}, {"action": "back"}]')"
run_check back_first "$(back_input '[{"action": "back"}]')"

if ! command -v jq >/dev/null 2>&1; then
  echo "jq is not installed: cannot assert the read_only and back cases" >&2
  exit 1
fi
failed=0
# expect NAME JQ_FILTER DESCRIPTION — the filter must be true of NAME's result.
expect() {
  if ! jq -e "$2" "$workdir/$1/result.json" >/dev/null 2>&1; then
    echo "FAIL $1: $3" >&2
    failed=1
  else
    echo "ok   $1: $3"
  fi
}
expect read_only '.status == "completed"' 'the check completed'
expect read_only 'any(.blocked_requests[]; .method == "POST" and (.url | startswith("https://httpbin.org/post")))' 'the form POST is in blocked_requests'
expect read_only 'all(.steps[]; .ok)' 'every step ran ok (a blocked submission fails no step)'
expect read_only '.final_url == "https://httpbin.org/forms/post"' 'final_url is still the form'
expect read_only 'all(.failed_requests[]; .method != "POST")' 'the blocked POST is not also a failed request'
expect read_write '.status == "completed"' 'the check completed'
expect read_write '.blocked_requests == []' 'blocked_requests is empty'
expect read_write '.final_url == "https://httpbin.org/post"' "final_url is httpbin's /post response"
expect back '.verdict == "pass"' 'going back from /html returns to the form'
expect back_first '.steps[0].ok == false and .steps[0].error == "there is no earlier page in this check to go back to"' 'back on the first page fails with the no-earlier-page error'
exit "$failed"
