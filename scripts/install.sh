#!/usr/bin/env bash
set -euo pipefail

REPOSITORY="yoyooyooo/agent-git-service"
VERSION_RE='^fork-[0-9]{8}\.[1-9][0-9]*(-rc[1-9][0-9]*)?$'
COMMANDS=(gh-server ags-edge ags-replication)

usage() {
  cat <<'EOF'
Usage:
  install.sh plan    [--version VERSION] [--allow-prerelease] [--prefix DIR]
  install.sh install [--version VERSION] [--allow-prerelease] [--prefix DIR] [--bin-dir DIR]
  install.sh upgrade [--version VERSION] [--allow-prerelease] [--prefix DIR] [--bin-dir DIR]

Without --version, GitHub Latest is resolved and must be an immutable stable release.
Use --version to select exact program bytes, including an RC. RC installation requires
--allow-prerelease.

One installation lives at ~/.ags/bin; there are no retained version slots.
A trusted adjacent installer is used when present; a standalone bootstrap obtains
its installer from Latest stable, independently of the requested binary version.
install/upgrade verify and replace programs, never restart services or migrate data.
EOF
}

die() { printf 'agent-git-service install: %s\n' "$*" >&2; exit 2; }
need() { command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"; }

action=${1:-}
case "$action" in plan|install|upgrade) shift ;; -h|--help|"") usage; exit 0 ;; *) die "unknown action: $action" ;; esac

version=
prefix="${HOME}/.ags"
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

if [[ -n "$version" ]]; then
  [[ "$version" =~ $VERSION_RE ]] || die "--version must match fork-YYYYMMDD.N[-rcN]"
else
  ((allow_prerelease == 0)) || die "--allow-prerelease requires an explicit --version"
fi
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

if [[ -z "$version" ]]; then
  release_json=$(gh api "repos/$REPOSITORY/releases/latest") || die "cannot resolve Latest stable release"
  version=$(python3 - "$release_json" <<'PY'
import json,re,sys
release=json.loads(sys.argv[1])
tag=str(release.get("tag_name",""))
if release.get("draft") or release.get("prerelease") or not release.get("immutable"):
    raise SystemExit("GitHub Latest is not an immutable stable release")
if not re.fullmatch(r"fork-[0-9]{8}\.[1-9][0-9]*",tag):
    raise SystemExit("GitHub Latest has an unsupported stable tag")
if not re.fullmatch(r"[0-9a-f]{40}",str(release.get("target_commitish",""))):
    raise SystemExit("GitHub Latest is not pinned to an exact source commit")
print(tag)
PY
  ) || die "Latest stable release identity rejected"
else
  release_json=$(gh api "repos/$REPOSITORY/releases/tags/$version") || die "release not found: $version"
fi

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
# The trap only removes this invocation's owner-only, freshly-created directory.
cleanup() {
  python3 - "$tmp" <<'PY'
import pathlib,shutil,sys
p=pathlib.Path(sys.argv[1])
if p.name.startswith('ags-installer.') and p.is_dir() and not p.is_symlink():
    shutil.rmtree(p)
PY
}
trap cleanup EXIT
metadata="$tmp/install-release.json"
installer="$tmp/install-release.py"
companion=""
if [[ -n ${BASH_SOURCE[0]:-} ]]; then
  companion="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/install-release.py"
fi
if [[ -n "$companion" && -f "$companion" && ! -L "$companion" ]]; then
  installer="$companion"
else
  # Old binary releases must not reintroduce their old multi-version installer.
  installer_release=$(gh api "repos/$REPOSITORY/releases/latest") || die "cannot resolve installer release"
  installer_source=$(python3 - "$installer_release" <<'PY'
import json,re,sys
r=json.loads(sys.argv[1]); source=r.get('target_commitish','')
if r.get('draft') or r.get('prerelease') or not r.get('immutable') or not re.fullmatch('[0-9a-f]{40}',source):
    raise SystemExit('installer release must be exact, immutable and stable')
print(source)
PY
  ) || die "installer source rejected"
  gh api "repos/$REPOSITORY/contents/scripts/install-release.py?ref=$installer_source" >"$metadata" || die "cannot fetch exact installer source"
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
fi
python3 - "$installer" <<'PY' || die "installer predates the single-root contract; use a current trusted checkout"
import pathlib,sys
if 'ags.installation.v2' not in pathlib.Path(sys.argv[1]).read_text():
    raise SystemExit('single-root installer is required')
PY

if [[ "$action" == install || "$action" == upgrade ]]; then
  if [[ -e "$bin_dir" && ! -d "$bin_dir" ]] || [[ -L "$bin_dir" ]]; then
    die "bin directory must be a real directory: $bin_dir"
  fi
  for command in "${COMMANDS[@]}"; do
    link="$bin_dir/$command"
    expected="$prefix/bin/$command"
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
  install) args+=(--install) ;;
  upgrade)
    [[ -f "$prefix/state/installation.json" && ! -L "$prefix/state/installation.json" ]] || die "upgrade requires an existing single-root installation; use install for first setup"
    args+=(--install)
    ;;
esac

python3 "$installer" "${args[@]}"

if [[ "$action" == install || "$action" == upgrade ]]; then
  [[ -f "$prefix/state/installation.json" ]] || die "installation succeeded without its current receipt"
  mkdir -p "$bin_dir"
  chmod 755 "$bin_dir"
  for command in "${COMMANDS[@]}"; do
    target="$prefix/bin/$command"
    [[ -x "$target" ]] || die "installed command missing: $command"
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
