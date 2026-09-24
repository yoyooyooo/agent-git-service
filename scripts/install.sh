#!/usr/bin/env bash
set -euo pipefail

REPOSITORY="yoyooyooo/agent-git-service"
VERSION_RE='^fork-[0-9]{8}\.[1-9][0-9]*(-rc[1-9][0-9]*)?$'
COMMANDS=(gh-server ags-edge ags-replication)

usage() {
  cat <<'EOF'
Usage:
  install.sh plan    --version VERSION [--allow-prerelease] [--prefix DIR]
  install.sh stage   --version VERSION [--allow-prerelease] [--prefix DIR]
  install.sh install --version VERSION [--allow-prerelease] [--prefix DIR] [--bin-dir DIR]
  install.sh upgrade --version VERSION [--allow-prerelease] [--prefix DIR] [--bin-dir DIR]

The wrapper bootstraps the exact Python installer from the immutable release tag.
install/upgrade atomically select prefix/current after verification. They do not
restart services or migrate live databases.
EOF
}

die() { printf 'agent-git-service install: %s\n' "$*" >&2; exit 2; }
need() { command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"; }

action=${1:-}
case "$action" in plan|stage|install|upgrade) shift ;; -h|--help|"") usage; exit 0 ;; *) die "unknown action: $action" ;; esac

version=
prefix="${HOME}/.local/lib/agent-git-service"
bin_dir="${HOME}/.local/bin"
allow_prerelease=0
while (($#)); do
  case "$1" in
    --version) version=${2:-}; shift 2 ;;
    --prefix) prefix=${2:-}; shift 2 ;;
    --bin-dir) bin_dir=${2:-}; shift 2 ;;
    --allow-prerelease) allow_prerelease=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

[[ -n "$version" && "$version" =~ $VERSION_RE ]] || die "an explicit fork-YYYYMMDD.N[-rcN] --version is required"
[[ -z ${MULTICA_TOKEN:-} ]] || die "workload context cannot install operator software"
need gh
need python3

python3 - <<'PY' || die "Python 3.9+ is required"
import sys
raise SystemExit(0 if sys.version_info >= (3, 9) else 1)
PY

prefix=$(python3 - "$prefix" <<'PY'
from pathlib import Path
import sys
p=Path(sys.argv[1]).expanduser().absolute()
if p.is_symlink() or p.resolve()!=p:
    raise SystemExit("installation prefix must be canonical and not a symlink")
print(p)
PY
)
bin_dir=$(python3 - "$bin_dir" <<'PY'
from pathlib import Path
import sys
p=Path(sys.argv[1]).expanduser().absolute()
if p.is_symlink():
    raise SystemExit("bin directory must not be a symlink")
print(p)
PY
)

release_json=$(gh api "repos/$REPOSITORY/releases/tags/$version") || die "release not found: $version"
source=$(python3 - "$release_json" "$version" "$allow_prerelease" <<'PY'
import json,re,sys
release=json.loads(sys.argv[1]); version=sys.argv[2]; allow=sys.argv[3]=="1"
if release.get("tag_name")!=version or release.get("draft") or not release.get("immutable"):
    raise SystemExit("release is draft, mutable, or mismatched")
if release.get("prerelease") and not allow:
    raise SystemExit("prerelease requires --allow-prerelease")
target=release.get("target_commitish","")
if not re.fullmatch(r"[0-9a-f]{40}",target):
    raise SystemExit("release is not pinned to an exact source commit")
print(target)
PY
) || die "release identity rejected"

ref_json=$(gh api "repos/$REPOSITORY/git/ref/tags/$version") || die "release tag cannot be read"
tag_source=$(python3 - "$ref_json" <<'PY'
import json,re,sys
obj=json.loads(sys.argv[1]).get("object",{})
if obj.get("type")!="commit" or not re.fullmatch(r"[0-9a-f]{40}",obj.get("sha","")):
    raise SystemExit("release tag must point directly to a commit")
print(obj["sha"])
PY
) || die "release tag rejected"
[[ "$source" == "$tag_source" ]] || die "release source and tag differ"

tmp=$(mktemp -d "${TMPDIR:-/tmp}/ags-installer.XXXXXXXX")
chmod 700 "$tmp"
# The bootstrap directory contains only public source metadata and is left for
# the host's normal temporary-file lifecycle. Do not turn cleanup into a broad
# deletion primitive.
metadata="$tmp/install-release.json"
installer="$tmp/install-release.py"
gh api "repos/$REPOSITORY/contents/scripts/install-release.py?ref=$source" >"$metadata" || die "cannot fetch exact installer source"
python3 - "$metadata" "$installer" <<'PY' || die "installer source identity rejected"
import base64,hashlib,json,os,sys
meta=json.load(open(sys.argv[1]))
if meta.get("type")!="file" or meta.get("path")!="scripts/install-release.py" or meta.get("encoding")!="base64":
    raise SystemExit("unexpected installer object")
encoded="".join(str(meta.get("content","")).split())
data=base64.b64decode(encoded,validate=True)
if len(data)>1024*1024:
    raise SystemExit("installer source is oversized")
oid=hashlib.sha1(b"blob "+str(len(data)).encode()+b"\0"+data).hexdigest()
if oid!=meta.get("sha"):
    raise SystemExit("installer Git blob identity mismatch")
fd=os.open(sys.argv[2],os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
with os.fdopen(fd,"wb") as out: out.write(data)
PY

if [[ "$action" == install || "$action" == upgrade ]]; then
  if [[ -e "$bin_dir" && ! -d "$bin_dir" ]] || [[ -L "$bin_dir" ]]; then
    die "bin directory must be a real directory: $bin_dir"
  fi
  for command in "${COMMANDS[@]}"; do
    link="$bin_dir/$command"
    expected="$prefix/current/bin/$command"
    if [[ -L "$link" ]]; then
      [[ "$(readlink "$link")" == "$expected" ]] || die "refusing to replace foreign symlink: $link"
    elif [[ -e "$link" ]]; then
      die "refusing to replace existing path: $link"
    fi
  done
fi

args=(--version "$version" --prefix "$prefix")
((allow_prerelease)) && args+=(--allow-prerelease)
case "$action" in
  plan) ;;
  stage) args+=(--install) ;;
  install) args+=(--install --activate) ;;
  upgrade)
    [[ -L "$prefix/current" ]] || die "upgrade requires an existing installer-owned current selector; use install for first setup"
    args+=(--install --activate)
    ;;
esac

python3 "$installer" "${args[@]}"

if [[ "$action" == install || "$action" == upgrade ]]; then
  [[ -L "$prefix/current" ]] || die "activation succeeded without an owned current selector"
  mkdir -p "$bin_dir"
  chmod 755 "$bin_dir"
  for command in "${COMMANDS[@]}"; do
    target="$prefix/current/bin/$command"
    [[ -x "$target" ]] || die "activated command missing: $command"
    link="$bin_dir/$command"
    if [[ -L "$link" ]]; then
      existing=$(readlink "$link")
      [[ "$existing" == "$target" ]] || die "refusing to replace foreign symlink: $link"
    elif [[ -e "$link" ]]; then
      die "refusing to replace existing path: $link"
    else
      ln -s "$target" "$link"
    fi
  done
  printf 'Installed %s. Commands linked in %s.\n' "$version" "$bin_dir"
  printf 'This did not restart a service or migrate live data.\n'
fi
