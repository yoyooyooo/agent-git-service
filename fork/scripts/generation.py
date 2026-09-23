#!/usr/bin/env python3
"""Materialize a reviewed public generation as Git objects, never edit a checkout.

The source must already descend from the frozen official baseline. This is a
publication/re-commit tool, not an upstream merge or an automatic capability
review. A private operator policy lists narrative substitutions and externalized
files. Production Go code is copied byte-for-byte from the accepted source.

The default is a read-only plan. --create-ref creates a NEW fork/* ref through
compare-and-swap; it never moves an existing ref, pushes, renames repositories,
changes visibility, or opens/deletes deployment data. Historical source remains
reachable only through the operator's pre-existing private refs.
"""
from __future__ import annotations

import argparse
import collections
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
from typing import Any

ROOT = Path(__file__).resolve().parents[2]
SHA = re.compile(r"[0-9a-f]{40}")


class Rejected(Exception):
    pass


def git(*args: str, data: bytes | None = None, env: dict[str, str] | None = None) -> bytes:
    result = subprocess.run(["git", "--no-replace-objects", *args], cwd=ROOT, input=data, env=env,
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=180)
    if result.returncode:
        raise Rejected("Git operation failed: " + args[0])
    return result.stdout


def resolve(ref: str) -> str:
    if ref.startswith("-"):
        raise Rejected("option-shaped source reference")
    value = git("rev-parse", "--verify", ref + "^{commit}").decode().strip()
    if not SHA.fullmatch(value):
        raise Rejected("source is not an exact SHA-1 commit")
    return value


def entries(ref: str) -> dict[str, tuple[str, str]]:
    result = {}
    for record in git("ls-tree", "-r", "-z", ref).split(b"\0"):
        if not record:
            continue
        metadata, raw_path = record.split(b"\t", 1)
        mode, kind, oid = metadata.decode().split()
        path = raw_path.decode("utf-8")
        if mode not in {"100644", "100755"} or kind != "blob" or not SHA.fullmatch(oid):
            raise Rejected("non-regular Git leaf requires explicit review")
        if path.startswith("/") or any(p in {"", ".", "..", ".git"} for p in path.split("/")) or any(ord(c) < 32 for c in path):
            raise Rejected("unsafe Git path")
        result[path] = (mode, oid)
    return result


def read_blobs(oids: list[str]) -> dict[str, bytes]:
    if not oids:
        return {}
    unique = list(dict.fromkeys(oids))
    packed = git("cat-file", "--batch", data=("\n".join(unique) + "\n").encode())
    offset = 0
    result = {}
    for expected in unique:
        end = packed.index(b"\n", offset)
        oid, kind, size = packed[offset:end].decode().split()
        start = end + 1
        count = int(size)
        body = packed[start:start + count]
        offset = start + count + 1
        if oid != expected or kind != "blob" or len(body) != count or packed[offset - 1:offset] != b"\n":
            raise Rejected("incomplete Git object read")
        result[oid] = body
    return result


def blob_oid(body: bytes) -> str:
    return hashlib.sha1(b"blob " + str(len(body)).encode() + b"\0" + body).hexdigest()


def narrative_path(path: str) -> bool:
    return (path.endswith("_test.go") or path.endswith(".md") or
            path.startswith(("testdata/", "docs/", "specs/")) or
            "/testdata/" in path or path.endswith(".example") or
            path.startswith("scripts/test_") or path.startswith("fork/scripts/test_"))


def allowed_externalization(path: str) -> bool:
    return path.endswith(".md") or path in {"scripts/edge-live-mini.py", "scripts/edge-ccs-canary.py", "t025-admin-positive-probe.txt"}


def group(path: str) -> str:
    if path.startswith("cli/"):
        return "client"
    if path.startswith(("internal/edge/", "cmd/ags-edge/")):
        return "edge"
    roots = ("internal/db/", "internal/gitstore/", "internal/gitbackend/", "internal/gittransport/",
             "internal/edgeprotocol/", "internal/snapshotstore/", "internal/delegationpolicy/",
             "internal/executioncontext/", "internal/sessionauthority/", "internal/operationcatalog/",
             "internal/operationconstraints/", "internal/workloadidentity/", "internal/providerlogprotocol/")
    if path.startswith(roots) or path in {"go.mod", "go.sum"}:
        return "foundations"
    provider_roots = ("internal/forgejointegration/", "internal/githubintegration/", "internal/gitlabintegration/",
                      "internal/integrations/", "internal/multicafailures/", "internal/multicaprojection/",
                      "internal/notifications/", "internal/projectionwatch/", "internal/providerlogbridge/")
    if path.startswith(provider_roots):
        return "providers"
    if path.startswith(("internal/", "auth/", "config/", "server/", "cmd/", "testdata/")) or path == "main.go":
        return "primary"
    if path.startswith(("docs/", "fork/", "specs/")) or path.endswith(".md"):
        return "governance"
    return "delivery"


GROUPS = [
    ("foundations", "feat(fork): preserve storage, authority and verified Git primitives"),
    ("providers", "feat(providers): retain governed projections and credential-safe delivery"),
    ("primary", "feat(primary): integrate scoped agent authority and exact effect recovery"),
    ("edge", "feat(edge): provide authorized local reads and observable incremental replication"),
    ("client", "feat(cli): retain primary API and credential compatibility"),
    ("delivery", "build(fork): use hosted verification and portable operating tools"),
    ("governance", "docs(fork): publish capability and generation contracts without operator history"),
]


def load_policy(path: Path, source: str, baseline: str) -> dict[str, Any]:
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077 or info.st_uid != os.getuid() or path.resolve().is_relative_to(ROOT):
        raise Rejected("policy must be an owner-only regular file outside the checkout")
    if info.st_size > 1 << 20:
        raise Rejected("policy is oversized")
    policy = json.loads(path.read_text())
    if set(policy) != {"version", "source", "baseline", "author", "externalize", "substitutions"} or policy["version"] != 1:
        raise Rejected("unsupported generation policy")
    if policy["source"] != source or policy["baseline"] != baseline:
        raise Rejected("policy source/baseline drift")
    author = policy["author"]
    if not isinstance(author, dict) or set(author) != {"name", "email"}:
        raise Rejected("public author identity must be explicit")
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9 ._-]{0,80}", author["name"]) or not re.fullmatch(r"[A-Za-z0-9+_.-]+@users\.noreply\.github\.com", author["email"]):
        raise Rejected("public author must use the selected GitHub noreply identity")
    paths = policy["externalize"]
    if not isinstance(paths, list) or len(set(paths)) != len(paths) or not all(isinstance(p, str) and allowed_externalization(p) for p in paths):
        raise Rejected("externalization cannot remove production code or tests")
    substitutions = policy["substitutions"]
    if not isinstance(substitutions, list) or len(substitutions) > 256:
        raise Rejected("invalid substitutions")
    for row in substitutions:
        if set(row) != {"from", "to"} or not all(isinstance(row[k], str) for k in row) or not row["from"] or row["from"] == row["to"]:
            raise Rejected("invalid narrative substitution")
        if any(c in row["from"] + row["to"] for c in "\0\r\n"):
            raise Rejected("substitutions must be single-line literals")
    return policy


def plan(source: str, baseline: str, policy: dict[str, Any]) -> tuple[dict, dict, dict]:
    original = entries(source)
    target = dict(original)
    baseline_entries = entries(baseline)
    for path in policy["externalize"]:
        if path not in original:
            raise Rejected("externalized path is missing; refresh policy")
        del target[path]
    eligible = [p for p in target if narrative_path(p) and original[p] != baseline_entries.get(p)]
    blobs = read_blobs([original[p][1] for p in eligible])
    transformed = {}
    changes = []
    applied = collections.Counter()
    for path in eligible:
        mode, oid = original[path]
        before = blobs[oid]
        if b"\0" in before:
            raise Rejected("binary narrative leaf requires manual review")
        text = before.decode("utf-8")
        for index, row in enumerate(policy["substitutions"]):
            count = text.count(row["from"])
            if count:
                text = text.replace(row["from"], row["to"])
                applied[index] += count
        after = text.encode()
        if after != before:
            new_oid = blob_oid(after)
            target[path] = (mode, new_oid)
            transformed[new_oid] = after
            changes.append({"path": path, "before": oid, "after": new_oid})
    production = [p for p in original if p.endswith(".go") and not p.endswith("_test.go")]
    if any(target.get(p) != original[p] for p in production):
        raise Rejected("generation changed or removed production Go code")
    changed = sorted(p for p in set(baseline_entries) | set(target) if baseline_entries.get(p) != target.get(p))
    summary = {
        "source": source, "baseline": baseline,
        "production_go_files_preserved": len(production),
        "transformed_narrative_paths": changes,
        "externalized_paths": policy["externalize"],
        "substitutions_applied": {str(k): v for k, v in sorted(applied.items())},
        "delta_paths": len(changed),
        "commit_groups": {name: sum(group(p) == name for p in changed) for name, _ in GROUPS},
        "public_release_approved": False,
        "claim_limit": "Content-preserving construction only. Final exact-head tests, semantic disposition and public-surface review remain required.",
    }
    return summary, target, transformed


def make_tree(leaves: dict[str, tuple[str, str]]) -> str:
    hierarchy: dict[str, Any] = {}
    for path, value in leaves.items():
        node = hierarchy
        parts = path.split("/")
        for component in parts[:-1]:
            node = node.setdefault(component, {})
        node[parts[-1]] = value

    def encode(node: dict) -> str:
        records = []
        for name, value in node.items():
            if isinstance(value, dict):
                metadata = "040000 tree " + encode(value)
            else:
                mode, oid = value
                metadata = mode + " blob " + oid
            records.append((metadata + "\t" + name).encode() + b"\0")
        return git("mktree", "-z", data=b"".join(records)).decode().strip()
    return encode(hierarchy)


def create(summary: dict, target: dict, transformed: dict, policy: dict, ref: str) -> dict:
    if not re.fullmatch(r"refs/heads/fork/[A-Za-z0-9][A-Za-z0-9._-]{0,100}", ref):
        raise Rejected("target must be an explicit new fork/* branch")
    existing = subprocess.run(["git", "show-ref", "--verify", "--quiet", ref], cwd=ROOT)
    if existing.returncode != 1:
        raise Rejected("target ref already exists or cannot be inspected")
    for oid, body in transformed.items():
        actual = git("hash-object", "-w", "--stdin", data=body).decode().strip()
        if actual != oid:
            raise Rejected("transformed object identity mismatch")
    baseline = summary["baseline"]
    current = entries(baseline)
    parent = baseline
    commits = []
    environment = dict(os.environ)
    for key in ("GIT_AUTHOR_DATE", "GIT_COMMITTER_DATE"):
        environment.pop(key, None)
    environment.update({"GIT_AUTHOR_NAME": policy["author"]["name"], "GIT_COMMITTER_NAME": policy["author"]["name"],
                        "GIT_AUTHOR_EMAIL": policy["author"]["email"], "GIT_COMMITTER_EMAIL": policy["author"]["email"]})
    delta = sorted(p for p in set(current) | set(target) if current.get(p) != target.get(p))
    for name, subject in GROUPS:
        paths = [p for p in delta if group(p) == name]
        if not paths:
            continue
        for path in paths:
            if path in target:
                current[path] = target[path]
            else:
                current.pop(path, None)
        tree = make_tree(current)
        parent = git("-c", "commit.gpgsign=false", "commit-tree", tree, "-p", parent,
                     data=(subject + "\n\nThis capability group belongs to an atomically verified fork generation.\n").encode(), env=environment).decode().strip()
        commits.append({"group": name, "commit": parent, "paths": len(paths)})
    if entries(parent) != target:
        raise Rejected("generation tree differs from reviewed target")
    if git("rev-list", "--merges", baseline + ".." + parent).strip():
        raise Rejected("generation is not linear")
    if summary["source"] != baseline:
        ancestry = subprocess.run(["git", "--no-replace-objects", "merge-base", "--is-ancestor", summary["source"], parent], cwd=ROOT).returncode
        if ancestry != 1:
            raise Rejected("private source ancestry is present or could not be excluded")
    git("update-ref", "-m", "create reviewed fork generation", ref, parent, "0" * 40)
    return {**summary, "created_ref": ref, "generation": parent, "commits": commits,
            "private_source_is_ancestor": False, "working_tree_modified": False}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", required=True)
    parser.add_argument("--baseline", required=True)
    parser.add_argument("--policy", required=True)
    parser.add_argument("--create-ref")
    args = parser.parse_args()
    source, baseline = resolve(args.source), resolve(args.baseline)
    if git("merge-base", source, baseline).decode().strip() != baseline:
        raise Rejected("reviewed source must already descend from frozen upstream")
    pinned = git("show", source + ":fork/UPSTREAM_BASELINE").decode().strip()
    if pinned != baseline:
        raise Rejected("source baseline file does not match selected upstream")
    policy = load_policy(Path(args.policy).absolute(), source, baseline)
    summary, target, transformed = plan(source, baseline, policy)
    output = create(summary, target, transformed, policy, args.create_ref) if args.create_ref else summary
    print(json.dumps(output, indent=2))
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (Rejected, ValueError, OSError, subprocess.TimeoutExpired) as error:
        print(json.dumps({"error": type(error).__name__, "message": str(error)}), file=sys.stderr)
        raise SystemExit(2)
