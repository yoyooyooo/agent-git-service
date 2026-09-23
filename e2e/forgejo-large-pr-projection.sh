#!/usr/bin/env bash
set -euo pipefail

# End-to-end validation for AGS -> Forgejo PR projection with a deterministic
# large binary payload. This script runs against an existing AGS server configured
# with Forgejo projection enabled. It never creates Forgejo PRs manually.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./lib.sh
source "$SCRIPT_DIR/lib.sh"

BASE_URL="$(strip_trailing_slash "${E2E_BASE_URL:-http://127.0.0.1:6666}")"
TOKEN="${AGS_E2E_TOKEN:-${E2E_TOKEN:-}}"
OWNER="${AGS_E2E_OWNER:-e2e}"
REPO="${AGS_E2E_REPO:-projection-large-$(date +%s)}"
BRANCH="${AGS_E2E_BRANCH:-agent/e2e-large-projection}"
RUNNER_LABEL="${AGS_E2E_RUNNER_LABEL:-operator-configured}"
RUNNER_IMAGE_REPO="${AGS_E2E_RUNNER_IMAGE_REPO:-operator-managed}"
PAYLOAD_MB="${AGS_E2E_PAYLOAD_MB:-38}"

if [[ -z "$TOKEN" ]]; then
  echo "SKIP forgejo-large-pr-projection: set AGS_E2E_TOKEN (or E2E_TOKEN) for an AGS user with repo create/write access" >&2
  exit 0
fi

require_cmd curl
require_cmd jq
require_cmd git
require_cmd python3

AUTH_HEADER=( -H "Authorization: token $TOKEN" )
JSON_HEADER=( -H "Content-Type: application/json" )
WORKDIR="$(mktemp -d)"
cleanup() { rm -rf "$WORKDIR"; }
trap cleanup EXIT

note "Forgejo large PR projection E2E baseline"
note "base_url=$BASE_URL owner=$OWNER repo=$REPO payload_mb=$PAYLOAD_MB"
note "runner_label=$RUNNER_LABEL"
note "runner_image_repo=$RUNNER_IMAGE_REPO"

viewer_login="$(curl_json 200 "${AUTH_HEADER[@]}" "$BASE_URL/api/v3/user" | jq -r '.login')"
repo_create_path="/api/v3/orgs/$OWNER/repos"

ensure_owner() {
  if [[ "$OWNER" == "$viewer_login" ]]; then
    repo_create_path="/api/v3/user/repos"
    return 0
  fi
  local code
  code="$(http_code "${AUTH_HEADER[@]}" "$BASE_URL/api/v3/orgs/$OWNER")"
  case "$code" in
    200) return 0 ;;
    404)
      curl_json 201 -X POST "${AUTH_HEADER[@]}" "${JSON_HEADER[@]}" -d "{\"login\":\"$OWNER\"}" "$BASE_URL/api/v3/user/orgs" >/dev/null
      ;;
    *)
      echo "unexpected owner lookup status: $code" >&2
      exit 1
      ;;
  esac
}

ensure_owner

repo_code="$(http_code "${AUTH_HEADER[@]}" "$BASE_URL/api/v3/repos/$OWNER/$REPO")"
if [[ "$repo_code" == "404" ]]; then
  curl_json 201 -X POST "${AUTH_HEADER[@]}" "${JSON_HEADER[@]}" \
    -d "{\"name\":\"$REPO\",\"auto_init\":true,\"private\":false}" "$BASE_URL$repo_create_path" >/dev/null
elif [[ "$repo_code" != "200" ]]; then
  echo "unexpected repo lookup status: $repo_code" >&2
  exit 1
fi

clone_url="$BASE_URL/$OWNER/$REPO.git"
git -c http.extraHeader="Authorization: token $TOKEN" clone "$clone_url" "$WORKDIR/repo" >/dev/null 2>&1
cd "$WORKDIR/repo"
git config user.name "ags-e2e"
git config user.email "ags-e2e@example.invalid"
git checkout -B "$BRANCH" >/dev/null 2>&1
mkdir -p ppt-images
python3 - <<PY
from pathlib import Path
mb = int("$PAYLOAD_MB")
chunk = b"PNG-LIKE-E2E-PAYLOAD\n" * 4096
remaining = mb * 1024 * 1024
idx = 1
while remaining > 0:
    size = min(remaining, 5 * 1024 * 1024)
    p = Path("ppt-images") / f"slide-{idx:03d}.png"
    with p.open("wb") as f:
        written = 0
        while written < size:
            data = chunk[:min(len(chunk), size - written)]
            f.write(data)
            written += len(data)
    remaining -= size
    idx += 1
PY
cat > projection-e2e-runner-baseline.md <<EOF
# Projection E2E runner baseline

- runner label: $RUNNER_LABEL
- runner image repo/worktree: $RUNNER_IMAGE_REPO
- payload: ${PAYLOAD_MB}MiB PNG-like binary files under ppt-images/

This file records the validation environment baseline. Projection success is
judged from AGS projection status and Forgejo PR projection facts, not from
runner-image health.
EOF
git add ppt-images projection-e2e-runner-baseline.md
git commit -m "test: add large projection e2e payload" >/dev/null
git -c http.extraHeader="Authorization: token $TOKEN" push origin "HEAD:$BRANCH" >/dev/null 2>&1
head_sha="$(git rev-parse HEAD)"

pr_json="$(curl_json 201 -X POST "${AUTH_HEADER[@]}" "${JSON_HEADER[@]}" \
  -d "{\"title\":\"E2E large Forgejo projection\",\"head\":\"$BRANCH\",\"base\":\"main\",\"body\":\"Large binary projection E2E; AGS remains authority.\"}" "$BASE_URL/api/v3/repos/$OWNER/$REPO/pulls")"
pr_number="$(jq -r '.number' <<<"$pr_json")"
assert_re "$pr_number" '^[0-9]+$'

note "created AGS PR #$pr_number at head_sha=$head_sha; waiting for async projection"
status_json=""
for _ in $(seq 1 120); do
  status_json="$(curl_json 200 "${AUTH_HEADER[@]}" "$BASE_URL/api/v3/repos/$OWNER/$REPO/projection/status")"
  phase="$(jq -r --argjson n "$pr_number" '.jobs[]? | select(.ags_pr_number == $n) | .phase' <<<"$status_json" | tail -1)"
  case "$phase" in
    projected) break ;;
    failed_terminal|failed_retryable)
      echo "projection failed for PR #$pr_number" >&2
      jq --argjson n "$pr_number" '.jobs[]? | select(.ags_pr_number == $n)' <<<"$status_json" >&2
      exit 1
      ;;
  esac
  sleep 2
done

job_json="$(jq -c --argjson n "$pr_number" '.jobs[]? | select(.ags_pr_number == $n)' <<<"$status_json" | tail -1)"
phase="$(jq -r '.phase' <<<"$job_json")"
assert_eq "$phase" "projected"
assert_eq "$(jq -r '.last_synced_ags_head_sha' <<<"$job_json")" "$head_sha"
assert_eq "$(jq -r '.remote_sha' <<<"$job_json")" "$head_sha"
external_url="$(jq -r '.external_url' <<<"$job_json")"
assert_re "$external_url" '^https?://'

note "verifying retry path is idempotent after projection"
retry_json="$(curl_json 202 -X POST "${AUTH_HEADER[@]}" "$BASE_URL/api/v3/repos/$OWNER/$REPO/projection/forgejo/pulls/$pr_number/retry")"
assert_eq "$(jq -r '.phase' <<<"$retry_json")" "projected"

ok "Forgejo large PR projection E2E passed: ags_pr=$pr_number external_url=$external_url head_sha=$head_sha"
