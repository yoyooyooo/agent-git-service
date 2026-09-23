#!/usr/bin/env python3
"""Verify legacy -> candidate SQLite migration using exact Git source archives.

Only synthetic fixtures are used. No live service, credential, database or Git
storage is read or modified. All generated files stay under a new private output
directory, which is retained as evidence rather than automatically deleted.
"""
from __future__ import annotations

import argparse
import io
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile

PROBE = r'''package main

import (
    "crypto/sha256"
    "encoding/hex"
    "encoding/json"
    "fmt"
    "os"
    "path/filepath"
    "reflect"
    "strings"

    "github.com/ngaut/agent-git-service/internal/db"
)

type Fingerprint struct { Rows int `json:"rows"`; SHA256 string `json:"sha256"` }
var queries = map[string]string{
    "users": "SELECT id, login, type, user_kind, status, site_admin FROM users ORDER BY id",
    "tokens": "SELECT id, user_id, name, value FROM tokens ORDER BY id",
    "repositories": "SELECT id, owner_id, full_name, name, git_storage_id, private, default_branch FROM repositories ORDER BY id",
    "issues": "SELECT id, repository_id, number, title, body, author_id, state FROM issues ORDER BY id",
    "pull_requests": "SELECT id, repository_id, head_repository_id, number, title, body, author_id, head_ref, base_ref, state FROM pull_requests ORDER BY id",
}
func main() {
    if err := run(); err != nil { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
}
func run() error {
    if len(os.Args) != 3 || (os.Args[1] != "seed" && os.Args[1] != "verify") { return fmt.Errorf("expected seed|verify PRIVATE_FIXTURE_DIRECTORY") }
    root := os.Args[2]
    database, err := db.Init("file:"+filepath.Join(root,"fixture.db"))
    if err != nil { return fmt.Errorf("initialize synthetic database: %w", err) }
    sqlDB, err := database.DB(); if err != nil { return err }; defer sqlDB.Close()
    if os.Args[1] == "seed" {
        user := db.User{Login:"upstream-fixture", Name:"Synthetic Upgrade Fixture", Type:db.TypeUser, UserKind:"human", Status:"active", SiteAdmin:true}
        if err := database.Create(&user).Error; err != nil { return err }
        identity := strings.Repeat("a",32)
        repository := db.Repository{OwnerID:user.ID, Name:"probe", FullName:"upstream-fixture/probe", DefaultBranch:"main", Private:true, GitStorageID:&identity}
        if err := database.Create(&repository).Error; err != nil { return err }
        token := db.Token{UserID:user.ID, Name:"synthetic-fixture", Value:"synthetic-fixture-not-a-live-credential"}
        if err := database.Create(&token).Error; err != nil { return err }
        issue := db.Issue{RepositoryID:repository.ID, Number:1, Title:"Preserve issue", Body:db.LargeText("synthetic issue body"), AuthorID:user.ID, State:db.StateOpen}
        if err := database.Create(&issue).Error; err != nil { return err }
        pr := db.PullRequest{RepositoryID:repository.ID, HeadRepositoryID:repository.ID, Number:2, Title:"Preserve PR", Body:db.LargeText("synthetic PR body"), AuthorID:user.ID, HeadRef:"feature", BaseRef:"main", State:db.StateOpen}
        if err := database.Create(&pr).Error; err != nil { return err }
    }
    fingerprints := map[string]Fingerprint{}
    for table, query := range queries {
        rows, err := sqlDB.Query(query); if err != nil { return err }
        columns, err := rows.Columns(); if err != nil { rows.Close(); return err }
        hash := sha256.New(); count := 0
        for rows.Next() {
            values := make([]any,len(columns)); targets := make([]any,len(columns))
            for i := range values { targets[i] = &values[i] }
            if err := rows.Scan(targets...); err != nil { rows.Close(); return err }
            for i, v := range values { if b, ok := v.([]byte); ok { values[i] = string(b) } }
            if err := json.NewEncoder(hash).Encode(values); err != nil { rows.Close(); return err }; count++
        }
        if err := rows.Err(); err != nil { rows.Close(); return err }; rows.Close()
        fingerprints[table] = Fingerprint{Rows:count,SHA256:hex.EncodeToString(hash.Sum(nil))}
    }
    baseline := filepath.Join(root,"baseline.json")
    if os.Args[1] == "seed" {
        data, err := json.MarshalIndent(fingerprints,"","  "); if err != nil { return err }
        return os.WriteFile(baseline,data,0600)
    }
    data, err := os.ReadFile(baseline); if err != nil { return err }
    var expected map[string]Fingerprint
    if err := json.Unmarshal(data,&expected); err != nil { return err }
    if !reflect.DeepEqual(expected,fingerprints) { return fmt.Errorf("legacy core identities or content changed during migration") }
    var integrity string
    if err := sqlDB.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" { return fmt.Errorf("integrity check: %q %v",integrity,err) }
    for _, table := range []string{"wiki_git_repair_obligations","wiki_search_projection_tasks","access_grants","authority_boundary_receipts"} {
        if !database.Migrator().HasTable(table) { return fmt.Errorf("missing expected table %s",table) }
    }
    report := map[string]any{"core_data_preserved":true,"replication_identity_preserved":true,"sqlite_integrity":integrity,"fingerprints":fingerprints,"live_data_used":false}
    data, err = json.MarshalIndent(report,"","  "); if err != nil { return err }
    return os.WriteFile(filepath.Join(root,"result.json"),data,0600)
}
'''


def run(command: list[str], cwd: Path, log: Path, timeout: int = 180) -> None:
    env = os.environ.copy()
    for name in list(env):
        if name.startswith(("AGS_", "FORGEJO_", "MULTICA_", "GIT_CONFIG_")) or name in {"DB_DSN", "CONTROL_PLANE_DSN", "ADMIN_TOKEN"}:
            env.pop(name, None)
    env["GIT_CONFIG_NOSYSTEM"] = "1"
    env["GIT_CONFIG_GLOBAL"] = os.devnull
    with log.open("ab") as output:
        result = subprocess.run(command, cwd=cwd, env=env, stdout=output, stderr=subprocess.STDOUT, timeout=timeout)
    if result.returncode:
        raise RuntimeError(f"fixture command failed (exit {result.returncode}); inspect private log {log}")


def archive(repo: Path, revision: str, destination: Path) -> str:
    sha = subprocess.check_output(["git", "rev-parse", "--verify", revision + "^{commit}"], cwd=repo, text=True).strip()
    if not re.fullmatch(r"[0-9a-f]{40}", sha):
        raise ValueError("expected a full commit SHA")
    payload = subprocess.check_output(["git", "archive", "--format=tar", sha], cwd=repo)
    destination.mkdir(mode=0o700)
    with tarfile.open(fileobj=io.BytesIO(payload)) as tar:
        members = tar.getmembers()
        for member in members:
            path = Path(member.name)
            if path.is_absolute() or ".." in path.parts or not (member.isfile() or member.isdir()):
                raise ValueError("source archive contains an unsafe entry")
        tar.extractall(destination, members=members)
    probe = destination / "cmd" / "upstream-sqlite-probe" / "main.go"
    if probe.exists():
        raise ValueError("source already contains the fixture-only probe")
    probe.parent.mkdir(mode=0o700)
    probe.write_text(PROBE)
    return sha


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--previous", required=True, help="Exact legacy source ref")
    parser.add_argument("--candidate", default="HEAD")
    parser.add_argument("--output", type=Path, required=True, help="New private evidence directory")
    args = parser.parse_args()
    os.umask(0o077)
    repo = Path(__file__).resolve().parents[1]
    output = args.output.expanduser().absolute()
    output.mkdir(mode=0o700, parents=False, exist_ok=False)
    fixture = output / "fixture"
    fixture.mkdir(mode=0o700)
    sources = {}
    for name, ref, mode in [("previous",args.previous,"seed"),("candidate",args.candidate,"verify")]:
        tree = output / name
        sources[name] = archive(repo,ref,tree)
        binary = output / (name + "-probe")
        run(["go","build","-buildvcs=false","-o",str(binary),"./cmd/upstream-sqlite-probe"],tree,output/(name+".log"))
        run([str(binary),mode,str(fixture)],tree,output/(name+".log"))
    result = json.loads((fixture/"result.json").read_text())
    result["sources"] = sources
    result["scope"] = "synthetic legacy schema and core records; no live database or deployment"
    (output/"receipt.json").write_text(json.dumps(result,indent=2)+"\n")
    print(json.dumps({"ok":True,"sources":sources,"core_data_preserved":True,"replication_identity_preserved":True,"sqlite_integrity":"ok","receipt":str(output/"receipt.json")}))


if __name__ == "__main__":
    main()
