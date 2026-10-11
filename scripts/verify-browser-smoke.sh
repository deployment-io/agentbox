#!/usr/bin/env bash
# Builds the verify-browser image and runs it twice with the runner's
# container settings (docs/VERIFY_BROWSER.md):
#   1. https://example.com with a text_visible "Example Domain" assertion —
#      expect status completed, verdict pass.
#   2. the same page, clicking its link to iana.org (outside allowed_hosts) —
#      expect a blocked navigation, a failed step and verdict fail.
# Needs Docker and network access. Run from anywhere in the repository.
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
  "assertions": [{"type": "text_visible", "text": "Example Domain"}],
  "timeout_ms": 45000
}'

run_check blocked '{
  "schema_version": 1,
  "url": "https://example.com/",
  "allowed_hosts": ["example.com"],
  "steps": [{"action": "click", "selector": "a[href*=\"iana.org\"]"}],
  "assertions": [{"type": "text_visible", "text": "Example Domain"}],
  "timeout_ms": 45000
}'

echo "Expected: 'pass' → verdict pass; 'blocked' → verdict fail, blocked_navigations has the iana.org URL, steps[0].ok false."
