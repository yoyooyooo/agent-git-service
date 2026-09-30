package gitstore

import "path/filepath"

// The operator-approved migration writes a private exact-OID list in the bare
// repository. This server-owned hook prevents an old checkout from pushing the
// retired history straight back after the migration succeeds.
func installRetiredBlobGuard(partsDir string) error {
	const script = `#!/bin/sh
set -eu
policy=$(git rev-parse --git-path ags-retired-blobs)
if [ ! -e "$policy" ] && [ ! -L "$policy" ]; then exit 0; fi
if [ ! -f "$policy" ] || [ -L "$policy" ] || [ ! -r "$policy" ]; then
    echo "error: invalid server artifact-retirement policy" >&2
    exit 1
fi
lines=$(wc -l < "$policy")
if [ "$lines" -le 0 ] || [ "$lines" -gt 32 ]; then
    echo "error: invalid server artifact-retirement policy size" >&2
    exit 1
fi
if grep -Eqv '^[0-9a-f]{40}$' "$policy"; then
    echo "error: invalid server artifact-retirement object list" >&2
    exit 1
else
    result=$?
    [ "$result" -eq 1 ] || exit 1
fi
umask 077
scan=$(mktemp "${TMPDIR:-/tmp}/ags-retirement-scan.XXXXXX") || exit 1
trap 'rm -f "$scan"' EXIT HUP INT TERM
while read -r oldrev newrev refname; do
    case "$refname" in
        refs/replace/*|refs/ags/retention/*|refs/retirement-import/*)
            echo "error: server-owned retirement references cannot be changed by Git push" >&2
            exit 1
            ;;
    esac
    [ "$newrev" = "0000000000000000000000000000000000000000" ] && continue
    if ! git --no-replace-objects rev-list --objects --no-object-names "$newrev" --not --all > "$scan"; then
        echo "error: unable to verify incoming history against retired artifacts" >&2
        exit 1
    fi
    if grep -Fx -f "$policy" "$scan" >/dev/null; then
        echo "error: incoming history reintroduces a retired artifact; move your work onto the current remote history" >&2
        exit 1
    else
        result=$?
        [ "$result" -eq 1 ] || exit 1
    fi
done
`
	return writeExecutableAtomically(filepath.Join(partsDir, "60-gh-server-retired-artifacts"), []byte(script))
}
