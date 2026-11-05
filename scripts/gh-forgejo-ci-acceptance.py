#!/usr/bin/env python3
"""Run official gh against the real AGS handler and synthetic Forgejo HTTP APIs.

Requires Python 3, Go, OpenSSL, Git and an explicitly selected official gh binary.
All listeners bind 127.0.0.1; credentials, SQLite, Git repositories and caches are
fresh fixture data. No system trust, host routing or production configuration is
changed. The generated directory is retained for inspection, including on failure.

This tests application/CLI compatibility. It does not claim a deployment, a real
Forgejo database test, or a network namespace. The provider/bridge are HTTP fixtures;
their owning Go tests separately exercise the real bridge and stored log offsets.
"""

import argparse
import hashlib
import http.server
import io
import json
import os
from pathlib import Path
import secrets
import shutil
import signal
import ssl
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import zipfile


REPOSITORY = "fixture/ci-acceptance"
PROVIDER_REPOSITORY = "ci/project"
WEB_ORIGIN = "https://forgejo.example.test"
HEAD = "a" * 40
NUMBER = 9
RUN = 10000 + NUMBER
TASK = 41
JOB = "unit / linux:go"
NOW = "2026-01-01T00:00:00Z"
DIAGNOSTIC = "AGS: output outside recorded steps"
MARKERS = ("setup-context", "successful-step-output", "failed-step-output", "cleanup-context")
LOG_LINES = tuple(f"{NOW} {marker}\n" for marker in MARKERS)
LOG_TEXT = "".join(LOG_LINES)


def sha256(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def require(condition, message):
    if not condition:
        raise AssertionError(message)


class ProviderFixture:
    """Intentionally unordered Forgejo inventory plus two bridge generations."""

    def __init__(self, provider_token, bridge_token):
        self.provider_token = provider_token
        self.bridge_token = bridge_token
        self.origin = ""
        self.mode = "legacy"
        self.requests = 0
        self.writes = 0
        self.credential_errors = 0
        # Stable order, deliberately unrelated to repository run number.
        self.numbers = list(range(1, 126, 2)) + list(range(2, 126, 2))

    def run(self, number):
        pr = number % 11 == 0
        return {
            "id": 10000 + number, "index_in_repo": number,
            "workflow_id": "other.yml" if number % 7 == 0 else "ci.yml",
            "title": f"fixture run {number}", "prettyref": "#7" if pr else ("feature" if number % 2 else "main"),
            "commit_sha": HEAD, "event": "pull_request_sync" if pr else "push",
            "status": "success" if number % 5 == 0 else "failure",
            "html_url": f"{self.origin}/{PROVIDER_REPOSITORY}/actions/runs/{number}",
            "created": NOW, "updated": NOW, "started": NOW, "stopped": NOW,
        }

    def steps(self):
        result = [
            {"name": "Install", "number": 1, "status": "completed", "conclusion": "success", "started_at": NOW, "completed_at": NOW},
            {"name": "Test", "number": 2, "status": "completed", "conclusion": "failure", "started_at": NOW, "completed_at": NOW},
        ]
        if self.mode != "legacy":
            result.append({"name": DIAGNOSTIC, "number": 3, "status": "completed", "conclusion": "failure", "started_at": None, "completed_at": None})
        return result

    def log(self):
        result = self.binding("ags.internal-provider-log.v1")
        result["text"] = LOG_TEXT
        if self.mode == "legacy":
            return result
        edges = [0]
        for line in LOG_LINES:
            edges.append(edges[-1] + len(line.encode()))
        steps = self.steps()
        steps[0]["ranges"] = [{"start": edges[1], "end": edges[2]}]
        steps[1]["ranges"] = [{"start": edges[2], "end": edges[3]}]
        steps[2]["ranges"] = [{"start": edges[0], "end": edges[1]}, {"start": edges[3], "end": edges[4]}]
        steps[2]["diagnostic"] = "unattributed"
        if self.mode == "corrupt":
            steps[1]["ranges"][0]["end"] += 1000
        result["step_logs"] = {"schema": "ags.internal-provider-step-logs.v1", "byte_length": edges[-1], "steps": steps}
        return result

    @staticmethod
    def binding(schema):
        return {"schema": schema, "repo": PROVIDER_REPOSITORY, "task_id": TASK, "run_number": NUMBER,
                "job_name": JOB, "head_sha": HEAD, "provider_pr": 0, "head_ref": "feature",
                "provider_ref": "refs/heads/feature", "event": "push"}

    def response(self, method, target, authorization):
        self.requests += 1
        parsed = urllib.parse.urlsplit(target)
        path, query = parsed.path, urllib.parse.parse_qs(parsed.query, keep_blank_values=True)
        bridge = path.startswith("/api/internal/provider-logs/")
        expected = "Bearer " + self.bridge_token if bridge else "token " + self.provider_token
        if authorization != expected:
            self.credential_errors += 1
            return 401, {"message": "fixture credential boundary rejected"}
        if method != "GET":
            self.writes += 1
            return 405, {"message": "fixture provider is read-only"}
        bridge_base = "/api/internal/provider-logs/repos/" + PROVIDER_REPOSITORY
        if bridge and path == bridge_base + "/runs":
            # mini must continue working with the deployed bridge generation.
            return 404, {"message": "not found"}
        if bridge and path == bridge_base + f"/runs/{RUN}/jobs":
            if self.mode == "legacy":
                return 404, {"message": "not found"}
            return 200, {"schema": "ags.internal-provider-jobs.v1", "repo": PROVIDER_REPOSITORY,
                         "run": str(RUN), "run_number": NUMBER, "head_sha": HEAD,
                         "jobs": [{"task_id": TASK, "name": JOB}]}
        if bridge and path == bridge_base + f"/tasks/{TASK}":
            if query.get("head_sha") != [HEAD] or query.get("head_ref") != ["feature"] or query.get("provider_pr") != ["0"]:
                return 409, {"message": "fixture binding mismatch"}
            if query.get("resource") == ["steps"]:
                return 200, {**self.binding("ags.internal-provider-steps.v1"), "steps": self.steps()}
            return 200, self.log()
        api_base = "/api/v1/repos/" + PROVIDER_REPOSITORY + "/actions/"
        if path == api_base + "runs":
            runs = [self.run(n) for n in self.numbers]
            for field, key in (("head_sha", "commit_sha"), ("run_number", "index_in_repo")):
                if field in query:
                    runs = [r for r in runs if str(r[key]) == query[field][0]]
            for field in ("event", "status"):
                values = query.get(field + "[]", query.get(field, []))
                if values:
                    runs = [r for r in runs if r[field] in values]
            page, limit = int(query.get("page", ["1"])[0]), int(query.get("limit", ["50"])[0])
            return 200, {"total_count": len(runs), "workflow_runs": runs[(page - 1) * limit:page * limit]}
        if path.startswith(api_base + "runs/"):
            # Older Forgejo has only the collection endpoint.
            return 404, {"message": "not found"}
        if path == api_base + "tasks":
            task = {"id": TASK, "run_number": NUMBER, "name": JOB, "workflow_id": "ci.yml",
                    "head_branch": "feature", "head_sha": HEAD, "status": "failure",
                    "url": f"{self.origin}/{PROVIDER_REPOSITORY}/actions/runs/{NUMBER}/jobs/0",
                    "created_at": NOW, "updated_at": NOW, "run_started_at": NOW}
            page = int(query.get("page", ["1"])[0])
            return 200, {"total_count": 1, "workflow_runs": [task] if page == 1 else []}
        return 404, {"message": "fixture route not found"}


def provider_server(fixture):
    class Handler(http.server.BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def do_GET(self):
            status, body = fixture.response(self.command, self.path, self.headers.get("Authorization", ""))
            data = json.dumps(body).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        do_POST = do_GET
        do_DELETE = do_GET
        do_PATCH = do_GET

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    fixture.origin = f"http://127.0.0.1:{server.server_port}"
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


class Acceptance:
    def __init__(self, args):
        self.args = args
        self.root = args.output.resolve()
        self.root.mkdir(mode=0o700, parents=True, exist_ok=False)
        self.source = args.source.resolve()
        self.secrets = [secrets.token_hex(32) for _ in range(3)]
        self.token, provider_token, bridge_token = self.secrets
        self.fixture = ProviderFixture(provider_token, bridge_token)
        self.provider = None
        self.process = None
        self.process_log = None
        self.origin = ""
        self.call_number = 0
        self.report = {"schema": "ags.official-gh-ci-acceptance.v1", "passed": False,
                       "fixture": "real AGS application handler with synthetic Forgejo/bridge HTTP endpoints",
                       "listeners": "127.0.0.1 only", "production_data": False, "production_credentials": False,
                       "checks": [], "started_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}
        self.env = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "HOME": str(self.root / "home"),
                    "LANG": "C.UTF-8", "NO_PROXY": "*", "GIT_CONFIG_NOSYSTEM": "1",
                    "GIT_CONFIG_GLOBAL": os.devnull, "GIT_TERMINAL_PROMPT": "0",
                    "GH_CONFIG_DIR": str(self.root / "gh-config"), "GH_PROMPT_DISABLED": "1",
                    "GH_NO_UPDATE_NOTIFIER": "1", "GH_NO_EXTENSION_UPDATE_NOTIFIER": "1",
                    "SSL_CERT_FILE": str(self.root / "cert.pem")}
        for directory in ("home", "gh-config", "application", "tmp"):
            (self.root / directory).mkdir(mode=0o700)
        self.env["TMPDIR"] = str(self.root / "tmp")

    def clean(self, value):
        text = str(value)
        for secret in self.secrets:
            text = text.replace(secret, "[REDACTED]")
        return text

    def command(self, command, *, env=None, timeout=60, cwd=None, accepted=(0,)):
        result = subprocess.run(command, cwd=cwd or self.root, env=env or self.env,
                                text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
        if result.returncode not in accepted:
            raise RuntimeError(f"{Path(command[0]).name} exited {result.returncode}: " + self.clean(result.stderr[-3000:]))
        return result

    def write(self, path, text):
        (self.root / path).write_text(text)
        (self.root / path).chmod(0o600)

    def setup(self):
        gh = str(self.args.gh.resolve())
        digest = sha256(gh)
        if self.args.gh_sha256:
            require(digest == self.args.gh_sha256.lower(), "selected gh binary SHA-256 mismatch")
        version = self.command([gh, "--version"]).stdout.splitlines()[0]
        require(version.startswith("gh version "), "selected binary did not identify as gh")
        self.report["gh"] = {"version": version, "sha256": digest, "expected_sha256_verified": bool(self.args.gh_sha256)}
        commit = self.command(["git", "rev-parse", "HEAD"], cwd=self.source).stdout.strip()
        dirty = bool(self.command(["git", "status", "--porcelain", "--untracked-files=no"], cwd=self.source).stdout.strip())
        self.report["source"] = {"commit": commit, "tracked_dirty": dirty}
        self.provider = provider_server(self.fixture)
        self.write("provider.token", self.fixture.provider_token)
        self.write("bridge.token", self.fixture.bridge_token)
        integration = {"ci": {"default_backend": "native", "backends": {"fixture": {
            "kind": "forgejo", "url": self.fixture.origin, "web_url": WEB_ORIGIN,
            "allow_http": True, "token_file": str(self.root / "provider.token"),
            "log_bridge": {"url": self.fixture.origin, "allow_http": True, "token_file": str(self.root / "bridge.token")}
        }}, "repositories": {REPOSITORY: {"backend": "fixture", "repository": PROVIDER_REPOSITORY, "required_checks": []}}}}
        if self.args.legacy_config:
            del integration["ci"]["backends"]["fixture"]["web_url"]
        self.report["legacy_config"] = self.args.legacy_config
        self.write("integrations.json", json.dumps(integration))
        self.write("tls.cnf", "[req]\ndistinguished_name=dn\nx509_extensions=v3\nprompt=no\n[dn]\nCN=127.0.0.1\n[v3]\nsubjectAltName=IP:127.0.0.1\nbasicConstraints=critical,CA:TRUE\nkeyUsage=digitalSignature,keyEncipherment,keyCertSign\n")
        self.command(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", str(self.root / "key.pem"),
                      "-out", str(self.root / "cert.pem"), "-days", "1", "-config", str(self.root / "tls.cnf")])
        helper = Path(__file__).with_name("gh-forgejo-ci-fixture.go.fixture")
        self.write("application-fixture.go", helper.read_text())
        go = str(self.args.go.resolve())
        # Reuse the installed, checksummed module cache without inheriting auth,
        # proxies or service environment. Building cannot fetch new dependencies.
        cache_env = {"PATH": self.env["PATH"], "HOME": str(Path.home())}
        cache = self.command([go, "env", "GOMODCACHE", "GOCACHE"], env=cache_env, cwd=self.source).stdout.splitlines()
        build_env = {**self.env, "GOMODCACHE": cache[0], "GOCACHE": cache[1], "GOPROXY": "off", "GOSUMDB": "off", "GOMAXPROCS": "3"}
        binary = self.root / "application-fixture"
        self.command([go, "build", "-p", "2", "-mod=readonly", "-trimpath", "-buildvcs=false",
                      "-ldflags=-X github.com/ngaut/agent-git-service/server.gitSHA=" + commit,
                      "-o", str(binary), str(self.root / "application-fixture.go")],
                     env=build_env, cwd=self.source, timeout=240)
        self.report["fixture_binary_sha256"] = sha256(binary)
        settings = {"root": str(self.root / "application"), "integrations": str(self.root / "integrations.json"),
                    "admin_token": self.token, "certificate": str(self.root / "cert.pem"), "key": str(self.root / "key.pem"),
                    "state": str(self.root / "listener.json")}
        self.write("settings.json", json.dumps(settings))
        self.process_log = (self.root / "application.log").open("w")
        self.process = subprocess.Popen([str(binary), str(self.root / "settings.json")], cwd=self.root / "application",
                                        env=self.env, stdout=self.process_log, stderr=subprocess.STDOUT, start_new_session=True)
        for _ in range(200):
            if self.process.poll() is not None:
                raise RuntimeError("fixture application exited: " + self.clean((self.root / "application.log").read_text()[-2500:]))
            try:
                self.origin = json.loads((self.root / "listener.json").read_text())["origin"]
                if self.api("/readyz")[0] == 200:
                    break
            except (FileNotFoundError, KeyError, ValueError, urllib.error.URLError):
                pass
            time.sleep(0.1)
        else:
            raise RuntimeError("fixture application readiness deadline")
        self.env["GH_HOST"] = urllib.parse.urlsplit(self.origin).netloc
        self.env["GH_ENTERPRISE_TOKEN"] = self.token
        status, body, _ = self.api("/api/v3/user/repos", method="POST", payload={"name": REPOSITORY.split("/")[1], "private": True, "auto_init": True})
        require(status == 201, f"fixture repository creation failed ({status})")
        require(body.get("full_name") == REPOSITORY, "repository was created under unexpected identity")

    def api(self, path, method="GET", payload=None, raw=False):
        data = None if payload is None else json.dumps(payload).encode()
        request = urllib.request.Request(self.origin + path, data=data, method=method,
                                         headers={"Authorization": "Bearer " + self.token, "Content-Type": "application/json"})
        context = ssl.create_default_context(cafile=str(self.root / "cert.pem"))
        try:
            response = urllib.request.urlopen(request, context=context, timeout=30)
        except urllib.error.HTTPError as response:
            body = response.read()
            return response.code, body if raw else json.loads(body), dict(response.headers)
        with response:
            body = response.read()
            return response.status, body if raw else json.loads(body), dict(response.headers)

    def gh(self, *args, success=True):
        self.call_number += 1
        # gh caches completed-run ZIPs by run identity and start time.
        # Every observation here must exercise the current fixture generation.
        cache = self.root / f"gh-cache-{self.call_number}"
        cache.mkdir(mode=0o700)
        env = {**self.env, "XDG_CACHE_HOME": str(cache)}
        result = self.command([str(self.args.gh.resolve()), *args], env=env, accepted=tuple(range(256)))
        require((result.returncode == 0) == success, "gh command outcome: " + self.clean(result.stderr[-2200:]))
        return result

    def check(self, name, fn):
        before = self.fixture.requests
        started = time.monotonic()
        try:
            detail = fn()
            self.report["checks"].append({"name": name, "passed": True, "seconds": round(time.monotonic() - started, 3),
                                          "provider_requests": self.fixture.requests - before, "evidence": detail})
        except Exception as exc:
            self.report["checks"].append({"name": name, "passed": False, "seconds": round(time.monotonic() - started, 3),
                                          "provider_requests": self.fixture.requests - before, "error": self.clean(exc)})

    def run_checks(self):
        repository_args = ["-R", REPOSITORY]
        prefix = "/api/v3/repos/" + REPOSITORY
        self.check("official gh authenticates to real application", lambda: self.check_identity())
        self.check("stable pagination through official gh limit 125", lambda: self.check_list(repository_args))
        self.check("conclusion and branch filters before pagination", lambda: self.check_filters(repository_args, prefix))
        self.check("API 100-item page, exact total and Link", lambda: self.check_pages(prefix))
        status, body, _ = self.api(prefix + "/actions/runs?per_page=100&page=1")
        require(status == 200, "cannot discover canonical target run")
        # Use the provider-number selector as the feature under test, not as
        # fixture setup. Locate the existing numeric identity by pagination.
        runs = body["workflow_runs"]
        page = 1
        while not any(r["run_number"] == NUMBER for r in runs) and page < 10:
            page += 1
            status, body, _ = self.api(prefix + f"/actions/runs?per_page=100&page={page}")
            require(status == 200, "cannot discover canonical target run page")
            if not body["workflow_runs"]:
                break
            runs.extend(body["workflow_runs"])
        target = next(r for r in runs if r["run_number"] == NUMBER)
        canonical = target["id"]
        self.report["target"] = {"run_number": NUMBER, "canonical_run_id": canonical, "head_sha": HEAD}
        self.check("typed number and explicit web URL preserve canonical identity", lambda: self.check_selectors(repository_args, canonical))
        self.check("wrong selector authority and typed writes are rejected", lambda: self.check_rejections(repository_args, prefix))
        self.check("legacy bridge preserves complete job logs", lambda: self.check_legacy(repository_args, prefix, canonical))
        self.fixture.mode = "modern"
        self.check("exact step logs and explicit unassigned output", lambda: self.check_steps(repository_args, prefix, canonical))
        self.fixture.mode = "corrupt"
        self.check("corrupt step ranges fall back without losing failure", lambda: self.check_corrupt(repository_args, canonical))
        self.check("provider credentials and read-only boundary", lambda: self.check_boundaries())

    def check_identity(self):
        body = json.loads(self.gh("api", "user").stdout)
        require(body["login"] == "fixture", "gh identity differs from generated fixture")
        return {"login": "fixture"}

    def check_list(self, repository_args):
        items = json.loads(self.gh("run", "list", *repository_args, "--limit", "125", "--json", "number,databaseId,status,conclusion").stdout)
        require([r["number"] for r in items] == list(range(125, 0, -1)), "run inventory is truncated, duplicated or unsorted")
        ids = [r["databaseId"] for r in items]
        require(len(set(ids)) == 125 and min(ids) > 2**52, "canonical mapping was bypassed or collided")
        return {"count": len(items), "first_number": items[0]["number"], "last_number": items[-1]["number"], "unique_ids": len(set(ids))}

    def check_filters(self, repository_args, prefix):
        items = json.loads(self.gh("run", "list", *repository_args, "--limit", "100", "--status", "failure", "--json", "number,conclusion").stdout)
        expected = [n for n in range(125, 0, -1) if n % 5]
        require([r["number"] for r in items] == expected, "failure conclusion filtering lost or reordered runs")
        require(all(r["conclusion"] == "failure" for r in items), "nonfailure run leaked through status filter")
        selected = [n for n in expected if self.fixture.run(n)["prettyref"] == "feature" and self.fixture.run(n)["event"] == "push"]
        status, page, _ = self.api(prefix + "/actions/runs?per_page=7&page=2&status=failure&branch=feature&event=push")
        require(status == 200 and page["total_count"] == len(selected), "filtered total_count does not describe the selected set")
        require([r["run_number"] for r in page["workflow_runs"]] == selected[7:14], "filters were applied after pagination")
        return {"failed_runs": len(items), "combined_filter_total": len(selected), "second_page_numbers": selected[7:14]}

    def check_pages(self, prefix):
        status, first, headers = self.api(prefix + "/actions/runs?per_page=100&page=1")
        require(status == 200 and first["total_count"] == 125 and len(first["workflow_runs"]) == 100, "100-item API page contract failed")
        link = next((v for k, v in headers.items() if k.lower() == "link"), "")
        require('rel="next"' in link and "page=2" in link, "next page Link missing or incorrect")
        status, second, _ = self.api(prefix + "/actions/runs?per_page=100&page=2")
        require(status == 200 and len(second["workflow_runs"]) == 25, "second API page lost rows")
        require([r["run_number"] for r in first["workflow_runs"] + second["workflow_runs"]] == list(range(125, 0, -1)), "API page boundary overlaps or gaps")
        return {"page_lengths": [100, 25], "total_count": 125, "next_link_present": True}

    def check_selectors(self, repository_args, canonical):
        observations = []
        for selector in (str(canonical), "forgejo:9", f"{WEB_ORIGIN}/{PROVIDER_REPOSITORY}/actions/runs/{NUMBER}"):
            body = json.loads(self.gh("run", "view", selector, *repository_args, "--json", "databaseId,number,headSha,status,conclusion").stdout)
            require(body["databaseId"] == canonical and body["number"] == NUMBER and body["headSha"] == HEAD, "selector changed the run identity")
            require(body["status"] == "completed" and body["conclusion"] == "failure", "selector changed the run result")
            observations.append(body["databaseId"])
        return {"same_canonical_ids": observations}

    def check_rejections(self, repository_args, prefix):
        rejected = ["forgejo:0", "https://untrusted.example.test/ci/project/actions/runs/9", f"{WEB_ORIGIN}/ci/other/actions/runs/9"]
        for selector in rejected:
            before = self.fixture.requests
            self.gh("run", "view", selector, *repository_args, "--json", "databaseId", success=False)
            require(self.fixture.requests == before, "invalid selector performed provider I/O")
        self.gh("run", "view", "9", *repository_args, "--json", "databaseId", success=False)
        before = self.fixture.requests
        status, _, _ = self.api(prefix + "/actions/runs/forgejo:9/rerun", method="POST")
        require(status in (404, 422) and self.fixture.requests == before, "typed selector widened provider write authority")
        return {"rejected_selectors": len(rejected), "bare_number_not_aliased": True, "typed_write_status": status}

    def check_legacy(self, repository_args, prefix, canonical):
        self.fixture.mode = "legacy"
        out = self.gh("run", "view", str(canonical), *repository_args, "--log-failed").stdout
        require(all(out.count(marker) == 1 for marker in MARKERS), "legacy bridge full-job fallback lost or repeated a line")
        require("UNKNOWN STEP" in out and "AGS_LOG_UNAVAILABLE" not in out, "legacy bridge was mistaken for missing logs")
        status, body, _ = self.api(prefix + f"/actions/runs/{canonical}/jobs")
        require(status == 200 and len(body["jobs"]) == 1, "legacy job inventory regressed")
        job = body["jobs"][0]
        status, raw, _ = self.api(prefix + f"/actions/jobs/{job['id']}/logs", raw=True)
        require(status == 200 and raw == LOG_TEXT.encode(), "raw job log endpoint changed its bytes")
        return {"full_job_markers": len(MARKERS), "raw_job_bytes": len(raw), "canonical_job_id": job["id"]}

    def check_steps(self, repository_args, prefix, canonical):
        out = self.gh("run", "view", str(canonical), *repository_args, "--log-failed").stdout
        require(MARKERS[1] not in out, "successful step leaked into --log-failed")
        require(all(out.count(marker) == 1 for marker in (MARKERS[0], MARKERS[2], MARKERS[3])), "failed step or unassigned output was lost")
        require("UNKNOWN STEP" not in out and DIAGNOSTIC in out, "step names or diagnostic provenance were lost")
        all_out = self.gh("run", "view", str(canonical), *repository_args, "--log").stdout
        require(all(all_out.count(marker) == 1 for marker in MARKERS), "complete logs lost or repeated a source line")
        status, archive, _ = self.api(prefix + f"/actions/runs/{canonical}/logs", raw=True)
        require(status == 200, "step archive fetch failed")
        with zipfile.ZipFile(io.BytesIO(archive)) as zipped:
            files = {entry.filename: zipped.read(entry) for entry in zipped.infolist() if not entry.is_dir()}
        expected = {1: LOG_LINES[1].encode(), 2: LOG_LINES[2].encode(), 3: (LOG_LINES[0] + LOG_LINES[3]).encode()}
        require(len(files) == 3, "archive is not an all-step archive")
        for number, data in expected.items():
            matches = [value for name, value in files.items() if Path(name).name.startswith(str(number) + "_")]
            require(matches == [data], "step archive bytes differ from the declared source ranges")
        require(sum(map(len, files.values())) == len(LOG_TEXT.encode()), "archive does not cover every original byte")
        status, body, _ = self.api(prefix + f"/actions/runs/{canonical}/jobs")
        require(status == 200, "step job metadata failed")
        steps = body["jobs"][0]["steps"]
        require([(s["number"], s["name"], s["conclusion"]) for s in steps] == [(1, "Install", "success"), (2, "Test", "failure"), (3, DIAGNOSTIC, "failure")], "jobs API and archive step identities disagree")
        return {"failed_log_markers": 3, "successful_marker_excluded": True, "archive_entries": sorted(files),
                "source_bytes": len(LOG_TEXT.encode()), "archive_content_bytes": sum(map(len, files.values()))}

    def check_corrupt(self, repository_args, canonical):
        out = self.gh("run", "view", str(canonical), *repository_args, "--log-failed").stdout
        require(all(out.count(marker) == 1 for marker in MARKERS), "corrupt range hid source log content")
        require("UNKNOWN STEP" in out and "AGS_LOG_UNAVAILABLE" not in out, "corrupt step metadata did not preserve whole-job fallback")
        return {"all_source_markers_retained": True}

    def check_boundaries(self):
        require(self.fixture.credential_errors == 0, "provider credential boundaries failed")
        require(self.fixture.writes == 0, "read acceptance attempted a provider write")
        return {"provider_writes": 0, "credential_errors": 0}

    def close(self):
        if self.process is not None and self.process.poll() is None:
            os.killpg(self.process.pid, signal.SIGTERM)
            try:
                self.process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                os.killpg(self.process.pid, signal.SIGKILL)
                self.process.wait(timeout=5)
        if self.process_log is not None:
            self.process_log.close()
        if self.provider is not None:
            self.provider.shutdown()
            self.provider.server_close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--gh", type=Path, required=True, help="explicit path to official gh")
    parser.add_argument("--gh-sha256", help="expected independently verified binary SHA-256")
    parser.add_argument("--go", type=Path, default=shutil.which("go"), help="Go toolchain (defaults to PATH)")
    parser.add_argument("--source", type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument("--legacy-config", action="store_true", help="omit new web_url field when reproducing failures on an older source")
    parser.add_argument("--output", type=Path, required=True, help="new private evidence directory; retained after execution")
    args = parser.parse_args()
    if args.go is None:
        parser.error("select a Go toolchain with --go")
    os.umask(0o077)
    acceptance = Acceptance(args)
    try:
        acceptance.setup()
        acceptance.run_checks()
        acceptance.report["passed"] = all(check["passed"] for check in acceptance.report["checks"])
    except Exception as exc:
        acceptance.report["error"] = acceptance.clean(exc)
    finally:
        acceptance.close()
        acceptance.report["finished_at"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        acceptance.write("receipt.json", json.dumps(acceptance.report, indent=2) + "\n")
    print(json.dumps({"passed": acceptance.report["passed"], "receipt": str(acceptance.root / "receipt.json"),
                      "checks": acceptance.report["checks"], "error": acceptance.report.get("error")}))
    return 0 if acceptance.report["passed"] else 1


if __name__ == "__main__":
    sys.exit(main())
