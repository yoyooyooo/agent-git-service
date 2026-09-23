#!/usr/bin/env bash
set -euo pipefail

mode=${1:-}
shift || true
repo_root=${AGS_SOURCE_REPO:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)}
source_sha=${GIT_SHA:-}
output=
tag=gh-server:local
prepare_only=${AGS_EXACT_SOURCE_PREPARE_ONLY:-0}
while (($#)); do
  case "$1" in
    --output) output=${2:?missing output}; shift 2 ;;
    --tag) tag=${2:?missing tag}; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

case "$mode" in verify|binary|docker) ;; *) echo "usage: $0 verify|binary|docker [--output path] [--tag image]" >&2; exit 2;; esac
[[ $source_sha =~ ^[0-9a-f]{40}$ ]] || { echo "GIT_SHA must be the exact lowercase 40-hex accepted source revision" >&2; exit 1; }
[[ -z ${GIT_REPLACE_REF_BASE:-} ]] || { echo "custom GIT_REPLACE_REF_BASE is forbidden for attributable builds" >&2; exit 1; }
git_exact() { GIT_NO_REPLACE_OBJECTS=1 git "$@"; }
repo_root=$(cd "$repo_root" && pwd -P)
[[ $(git_exact -C "$repo_root" rev-parse --show-toplevel) == "$repo_root" ]] || { echo "source repo root is not canonical" >&2; exit 1; }
[[ -z $(git_exact -C "$repo_root" for-each-ref --format='%(refname)' refs/replace/) ]] || { echo "source repository contains replace refs" >&2; exit 1; }
[[ $(git_exact -C "$repo_root" rev-parse --verify HEAD) == "$source_sha" ]] || { echo "GIT_SHA does not match checkout HEAD" >&2; exit 1; }
[[ -z $(git_exact -C "$repo_root" status --porcelain=v1 --untracked-files=all) ]] || { echo "source checkout must be clean before an attributable build" >&2; exit 1; }
if git_exact -C "$repo_root" ls-files -v | grep -Ev '^H ' >/dev/null; then
  echo "source index contains assume-unchanged, skip-worktree, or non-canonical tracked state" >&2
  exit 1
fi
source_tree=$(git_exact -C "$repo_root" rev-parse "${source_sha}^{tree}")
[[ $source_tree =~ ^[0-9a-f]{40}$ ]] || { echo "source tree unavailable" >&2; exit 1; }
# ls-tree normalizes certain malformed raw tree modes. Strict object validation
# must run first so a literal badFilemode object cannot be attributed to the
# accepted revision while appearing as a regular leaf in porcelain output.
fsck_status=0
fsck_output=$(git_exact -C "$repo_root" fsck --strict --no-dangling "$source_sha" 2>&1) || fsck_status=$?
if ((fsck_status != 0)) || [[ -n $fsck_output ]]; then
  echo "accepted source object graph failed strict validation: $fsck_output" >&2
  exit 1
fi
invalid_tree_entry=$(git_exact -C "$repo_root" ls-tree -r "$source_sha" | awk '$1 != "100644" && $1 != "100755" && !found { print; found=1 }')
if [[ -n $invalid_tree_entry ]]; then
  echo "accepted source tree contains a non-regular leaf: $invalid_tree_entry" >&2
  exit 1
fi

if [[ ${AGS_EXACT_SOURCE_TEST_MODE:-0} == 1 && -n ${AGS_EXACT_SOURCE_AFTER_VERIFY_HOOK:-} ]]; then
  bash -lc "$AGS_EXACT_SOURCE_AFTER_VERIFY_HOOK"
fi
if [[ $mode == verify ]]; then
  printf 'source_revision=%s\nsource_tree=%s\n' "$source_sha" "$source_tree"
  exit 0
fi

canonical_temp_dir() {
  local candidate=$1
  local label=$2
  [[ -e $candidate ]] || { echo "$label does not exist: $candidate" >&2; return 1; }
  [[ -d $candidate ]] || { echo "$label is not a directory: $candidate" >&2; return 1; }
  local canonical
  canonical=$(cd "$candidate" && pwd -P) || { echo "$label cannot be canonicalized: $candidate" >&2; return 1; }
  [[ -w $canonical ]] || { echo "$label is not writable: $canonical" >&2; return 1; }
  printf '%s\n' "$canonical"
}

inside_source_repo() {
  local candidate_prefix=${1%/}/
  local repo_prefix=${repo_root%/}/
  [[ $candidate_prefix == "$repo_prefix"* ]]
}

if [[ ${AGS_EXACT_SOURCE_TMPDIR+x} == x ]]; then
  audit_temp_root=$(canonical_temp_dir "$AGS_EXACT_SOURCE_TMPDIR" "AGS_EXACT_SOURCE_TMPDIR")
  if inside_source_repo "$audit_temp_root"; then
    echo "AGS_EXACT_SOURCE_TMPDIR must be outside source repo: $audit_temp_root" >&2
    exit 1
  fi
else
  audit_temp_root=$(canonical_temp_dir "${TMPDIR:-/tmp}" "TMPDIR")
  if inside_source_repo "$audit_temp_root"; then
    audit_temp_root=$(canonical_temp_dir /tmp "fallback TMPDIR")
    if inside_source_repo "$audit_temp_root"; then
      echo "fallback TMPDIR must be outside source repo: $audit_temp_root" >&2
      exit 1
    fi
  fi
fi

audit_root=$(mktemp -d "${audit_temp_root%/}/ags-exact-source.${source_sha}.XXXXXX")
audit_root=$(cd "$audit_root" && pwd -P)
if inside_source_repo "$audit_root"; then
  echo "audit root must be outside source repo: $audit_root" >&2
  exit 1
fi
source_dir=$audit_root/source
mkdir -p "$source_dir"
git_exact -C "$repo_root" archive --format=tar "$source_sha" | tar -xf - -C "$source_dir"
printf '%s\n' "$source_sha" > "$source_dir/.ags-source-revision"
printf '%s\n' "$source_tree" > "$source_dir/.ags-source-tree"

verify_checkout_unchanged() {
  [[ -z $(git_exact -C "$repo_root" for-each-ref --format='%(refname)' refs/replace/) ]] || { echo "source repository gained replace refs during attributable build" >&2; return 1; }
  [[ $(git_exact -C "$repo_root" rev-parse --verify HEAD) == "$source_sha" ]] || { echo "checkout HEAD changed during attributable build" >&2; return 1; }
  [[ -z $(git_exact -C "$repo_root" status --porcelain=v1 --untracked-files=all) ]] || { echo "checkout changed during attributable build" >&2; return 1; }
  ! git_exact -C "$repo_root" ls-files -v | grep -Ev '^H ' >/dev/null || { echo "source index flags changed during attributable build" >&2; return 1; }
}

case "$mode" in
  binary)
    [[ -n $output ]] || output=$repo_root/gh-server
    (cd "$source_dir" && CGO_ENABLED=${CGO_ENABLED:-1} go build -trimpath \
      -ldflags="-X github.com/ngaut/agent-git-service/server.gitSHA=${source_sha}" \
      -o "$audit_root/gh-server" ./cmd/gh-server)
    verify_checkout_unchanged
    install -m 0755 "$audit_root/gh-server" "$output"
    printf 'source_revision=%s\nsource_tree=%s\naudit_root=%s\noutput=%s\n' "$source_sha" "$source_tree" "$audit_root" "$output"
    ;;
  docker)
    verify_checkout_unchanged
    if [[ $prepare_only == 1 ]]; then
      printf 'source_revision=%s\nsource_tree=%s\naudit_root=%s\ncontext=%s\n' "$source_sha" "$source_tree" "$audit_root" "$source_dir"
      exit 0
    fi
    docker build --build-arg "GIT_SHA=${source_sha}" --build-arg "GIT_TREE=${source_tree}" -t "$tag" "$source_dir"
    verify_checkout_unchanged
    printf 'source_revision=%s\nsource_tree=%s\naudit_root=%s\nimage=%s\n' "$source_sha" "$source_tree" "$audit_root" "$tag"
    ;;
esac
