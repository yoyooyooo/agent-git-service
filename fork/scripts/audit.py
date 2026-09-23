#!/usr/bin/env python3
"""Read-only fork inventory and publication preflight; never rewrites Git or files.

JSON findings contain locations/rule names, never matched values or credentials.
An inventory is a triage ledger, not semantic review or permission to publish.
"""
from __future__ import annotations

import argparse
import collections
import json
import os
from pathlib import Path
import re
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[2]


def git(*args: str, data: bytes | None = None) -> bytes:
    result = subprocess.run(["git", *args], cwd=ROOT, input=data,
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=120)
    if result.returncode:
        raise RuntimeError("git operation failed: " + args[0])
    return result.stdout


def resolve(ref: str) -> str:
    if ref.startswith("-"):
        raise ValueError("option-shaped Git reference")
    result = git("rev-parse", "--verify", ref + "^{commit}").decode().strip()
    if not re.fullmatch(r"[0-9a-f]{40}", result):
        raise ValueError("expected exact SHA-1 commit")
    return result


def category(path: str) -> str:
    if path.startswith(("docs/", "specs/", "fork/")) or path.endswith(".md"):
        return "documentation-and-governance"
    if path.startswith((".github/", "scripts/", "deploy/", "e2e/")) or path in {"Makefile", "Dockerfile", ".env.example"}:
        return "build-and-operations"
    if path.startswith("cli/"):
        return "client-compatibility"
    if re.search(r"(?:edge|snapshotstore|replication|gitbackend)", path):
        return "edge-and-replication"
    if re.search(r"(?:access_grant|authority|delegat|execution.context|principal|session|workload|operation.catalog|operation.constraints)", path):
        return "authority-and-grants"
    if re.search(r"(?:forgejo|gitlab|githubintegration|multica|projection|outbound|notification|provider|integration)", path):
        return "provider-and-delivery"
    if path.startswith(("internal/", "server/", "config/", "auth/", "cmd/")) or path in {"go.mod", "go.sum"}:
        return "primary-platform-and-storage"
    return "unclassified"


def changed_paths(base: str, source: str) -> list[tuple[str, str]]:
    raw = git("diff", "--no-renames", "--name-status", "-z", base, source).decode().split("\0")
    if raw[-1] == "":
        raw.pop()
    if len(raw) % 2:
        raise ValueError("incomplete Git path inventory")
    return list(zip(raw[::2], raw[1::2]))


def inventory(base: str, source: str, details: bool) -> dict:
    commits = git("rev-list", "--reverse", base + ".." + source).decode().splitlines()
    ledger = []
    counts: collections.Counter[str] = collections.Counter()
    for commit in commits:
        paths = [p for p in git("diff-tree", "--no-commit-id", "--no-renames", "--name-only", "-z", "-r", commit).decode().split("\0") if p]
        groups = sorted({category(p) for p in paths}) or ["metadata-only"]
        counts.update(groups)
        row = {"commit": commit, "capabilities": groups, "changed_paths": len(paths),
               "semantic_review": "pending"}
        if details:
            row["paths"] = paths
        ledger.append(row)
    paths = changed_paths(base, source)
    return {"scope": "complete-fork-history-triage", "baseline": base, "source": source,
            "commit_count": len(commits), "ledger_count": len(ledger),
            "merge_count": len(git("rev-list", "--merges", base + ".." + source).splitlines()),
            "path_count": len(paths), "path_statuses": dict(collections.Counter(s for s, _ in paths)),
            "capability_commit_counts_nonexclusive": dict(sorted(counts.items())),
            "paths": [{"status": s, "path": p, "capability": category(p)} for s, p in paths],
            "commits": ledger, "claim_limit": "Every commit is inventoried; none is auto-approved."}


def privacy_rules(policy_path: str | None) -> dict[str, re.Pattern[str]]:
    rules = {
        "operator-home-path": r"/(?:Users|home)/(?!example(?:/|\b)|runner(?:/|\b)|user(?:/|\b))[^\s/\"'<>]+/",
        "private-mesh-domain": r"\b(?!example\.ts\.net\b)[a-zA-Z0-9.-]+\.ts\.net\b",
        "private-mesh-address": r"\b(?!100\.64\.0\.0/10\b)100\.(?:6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\.[0-9]{1,3}\.[0-9]{1,3}\b",
        "private-key-material": r"-----BEGIN (?:RSA |EC |OPENSSH |DSA )?PRIVATE KEY-----",
        "credential-in-url": r"https?://[^\s/@:\"']+:[^\s/@\"']+@",
    }
    literal_names: set[str] = set()
    if policy_path:
        extra = json.loads(Path(policy_path).read_text())
        for name, values in extra.get("literal_patterns", {}).items():
            if not re.fullmatch(r"[a-z0-9-]+", name) or not isinstance(values, list) or not all(isinstance(v, str) and v for v in values):
                raise ValueError("invalid private policy")
            if name in rules:
                raise ValueError("private policy cannot override a built-in rule")
            rules[name] = "|".join(re.escape(v) for v in values)
            literal_names.add(name)
    # Hostnames and account examples can appear in uppercase normalization
    # cases. Changing their case must not evade an operator-owned deny rule.
    return {k: re.compile(v, re.IGNORECASE if k in literal_names else 0) for k, v in rules.items()}


def reviewed_fixtures(source: str) -> dict[tuple[str, str, str], str]:
    manifest_path = "fork/publication-fixtures.json"
    record = git("ls-tree", source, "--", manifest_path).decode().strip()
    if not record:
        return {}
    configuration = json.loads(git("show", source + ":" + manifest_path))
    if set(configuration) != {"version", "examples"} or configuration["version"] != 1:
        raise ValueError("invalid reviewed fixture manifest")
    approved = {}
    for row in configuration["examples"]:
        if set(row) != {"path", "object", "rule", "reason"}:
            raise ValueError("invalid reviewed fixture entry")
        path, oid, rule, reason = row["path"], row["object"], row["rule"], row["reason"]
        test_path = isinstance(path, str) and (path.endswith("_test.go") or (path.startswith(("scripts/test_", "fork/scripts/test_")) and path.endswith(".py")))
        if not test_path or path.startswith("/") or ".." in path.split("/"):
            raise ValueError("reviewed examples must be exact test source paths")
        if not isinstance(oid, str) or not re.fullmatch(r"[0-9a-f]{40}", oid) or rule not in {"credential-in-url", "private-key-material"}:
            raise ValueError("only exact synthetic URL/key-shape fixtures may be reviewed; operator data cannot be suppressed")
        if not isinstance(reason, str) or not 20 <= len(reason) <= 500:
            raise ValueError("a bounded manual fixture-review reason is required")
        current = git("ls-tree", source, "--", path).decode().strip().split()
        if len(current) != 4 or current[1] != "blob" or current[2] != oid:
            raise ValueError("reviewed fixture content drift; re-review the changed fixture")
        key = (oid, path, rule)
        if key in approved:
            raise ValueError("duplicate reviewed fixture")
        approved[key] = reason
    return approved


def publication(base: str, source: str, policy_path: str | None, *, tree_only: bool = False) -> dict:
    rules = privacy_rules(policy_path)
    approved = reviewed_fixtures(source)
    entries: list[tuple[str, str]] = []
    if tree_only:
        changed = {path for status, path in changed_paths(base, source) if status != "D"}
        for record in git("ls-tree", "-r", "-z", source).decode().split("\0"):
            if not record:
                continue
            metadata, path = record.split("\t", 1)
            mode, kind, oid = metadata.split()
            if path in changed:
                if kind != "blob" or mode not in {"100644", "100755"}:
                    raise ValueError("non-regular changed tree entry requires manual review")
                entries.append((oid, path))
    else:
        unique: dict[str, str] = {}
        for line in git("rev-list", "--objects", base + ".." + source).decode().splitlines():
            oid, _, path = line.partition(" ")
            unique.setdefault(oid, path)
        entries = list(unique.items())
    data = git("cat-file", "--batch", data=("\n".join(oid for oid, _ in entries) + "\n").encode()) if entries else b""
    offset = 0
    findings = []
    reviewed = []
    inspected = 0
    for expected, path in entries:
        end = data.index(b"\n", offset)
        oid, kind, size_text = data[offset:end].decode().split()
        size = int(size_text)
        start = end + 1
        body = data[start:start + size]
        offset = start + size + 1
        if oid != expected or len(body) != size or offset > len(data):
            raise ValueError("incomplete object inspection")
        if kind not in {"blob", "commit", "tag"}:
            continue
        inspected += 1
        if b"\0" in body:
            findings.append({"object": oid, "path": path, "rule": "binary-manual-review-required"})
            continue
        text = body.decode("utf-8", "replace")
        for name, pattern in rules.items():
            match = pattern.search(text)
            if match:
                item = {"object": oid, "path": path, "kind": kind, "rule": name,
                        "line": text.count("\n", 0, match.start()) + 1}
                if kind == "blob" and (oid, path, name) in approved:
                    reviewed.append(item)
                else:
                    findings.append(item)
    return {"scope": "committed-fork-tree-only" if tree_only else "all-objects-reachable-after-baseline", "baseline": base, "source": source,
            "commit_metadata_scanned": not tree_only,
            "objects_inspected": inspected, "operator_policy_used": bool(policy_path),
            "findings": findings, "finding_count": len(findings),
            "reviewed_synthetic_examples": reviewed, "reviewed_example_count": len(reviewed),
            "raw_pattern_count": len(findings) + len(reviewed),
            "rules": dict(collections.Counter(f["rule"] for f in findings)),
            "git_content_gate": "blocked" if findings else "no-pattern-findings",
            "public_release_approved": False,
            "claim_limit": "Pattern scan is not secret assurance. GitHub refs, PRs, logs, artifacts, author metadata and manual review remain separate gates."}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=["inventory", "tree", "publication", "verify"])
    parser.add_argument("--source", default="HEAD")
    parser.add_argument("--base")
    parser.add_argument("--details", action="store_true")
    parser.add_argument("--report", help="Create an owner-only report outside the source tree; never overwrite")
    parser.add_argument("--private-policy", help="Operator-only file outside this repository; never print its values")
    args = parser.parse_args()
    base = resolve(args.base or (ROOT / "fork/UPSTREAM_BASELINE").read_text().strip())
    source = resolve(args.source)
    if git("merge-base", base, source).decode().strip() != base:
        raise ValueError("source does not descend from frozen upstream")
    if args.mode == "inventory":
        output = inventory(base, source, args.details)
    elif args.mode in {"tree", "publication"}:
        if args.private_policy and Path(args.private_policy).resolve().is_relative_to(ROOT):
            raise ValueError("private publication policy must be outside the source tree")
        output = publication(base, source, args.private_policy, tree_only=args.mode == "tree")
    else:
        merges = git("rev-list", "--merges", base + ".." + source).decode().splitlines()
        dirty = bool(git("status", "--porcelain=v1", "--untracked-files=all"))
        if source != resolve("HEAD") or merges or dirty:
            raise ValueError("verification requires clean exact HEAD and linear fork ancestry")
        output = {"source": source, "baseline": base, "clean": True, "linear": True,
                  "claim_limit": "Source identity only; neither tests nor publication approval."}
    encoded = json.dumps(output, indent=2, ensure_ascii=False) + "\n"
    if args.report:
        destination = Path(args.report).absolute()
        if destination.resolve().is_relative_to(ROOT) or not destination.parent.is_dir():
            raise ValueError("report requires an existing parent outside the source tree")
        descriptor = os.open(destination, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600)
        with os.fdopen(descriptor, "w") as report:
            report.write(encoded)
        summary = {k: v for k, v in output.items() if k not in {"commits", "paths", "findings"}}
        summary["report_created"] = True
        print(json.dumps(summary, indent=2))
    else:
        print(encoded, end="")
    return 1 if args.mode in {"tree", "publication"} and output["finding_count"] else 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (ValueError, RuntimeError, OSError, subprocess.TimeoutExpired) as exc:
        print(json.dumps({"error": type(exc).__name__, "message": str(exc)}), file=sys.stderr)
        raise SystemExit(2)
