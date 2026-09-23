#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
module=$(go list -m)
actual=$(go list ./... | awk -v prefix="$module" '{ sub("^" prefix, "."); print }' | sort)
declared=$(awk '/^[[:space:]]+packages: / { for (i=2;i<=NF;i++) print $i }' .github/workflows/ci.yml | sort)
missing=$(comm -23 <(printf '%s\n' "$actual") <(printf '%s\n' "$declared"))
extra=$(comm -13 <(printf '%s\n' "$actual") <(printf '%s\n' "$declared" | sort -u))
duplicates=$(printf '%s\n' "$declared" | uniq -d)
if [[ -n "$missing$extra$duplicates" ]]; then
  printf 'CI package inventory mismatch\nMissing:\n%s\nUnknown:\n%s\nDuplicated:\n%s\n' "$missing" "$extra" "$duplicates" >&2
  exit 1
fi
printf 'CI covers all %s root-module packages exactly once.\n' "$(printf '%s\n' "$actual" | wc -l | tr -d ' ')"
