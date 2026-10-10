#!/usr/bin/env node
/** Local parity checks for the remaining official-gh gaps.
 * HTTP API covers the server contract. Official gh is used when the local TLS
 * front can complete a handshake; Darwin stock gh speaks HTTPS to :port hosts.
 * Does not touch the running AGS. */
import { spawn, spawnSync } from "node:child_process";
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync, rmSync, createWriteStream } from "node:fs";
import { randomBytes } from "node:crypto";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import http from "node:http";
import https from "node:https";

const root = resolve(new URL(".", import.meta.url).pathname, "..");
const ghBin = process.env.GH_BIN || "gh";
const work = mkdtempSync(join(tmpdir(), "ags-gh-parity-2-"));
const serverHome = join(work, "server");
const checkout = join(work, "project");
mkdirSync(serverHome, { mode: 0o700 });
const token = randomBytes(24).toString("hex");
const port = process.env.AGS_E2E_PORT || "18250";
const tlsPort = process.env.AGS_E2E_TLS_PORT || "18251";
const base = `http://127.0.0.1:${port}`;
const repo = "fixture/parity";
const report = { schema: "ags.gh-parity-2.local.v1", checks: [], work };
let server;
let serverLog = "";
let tlsProxy;

function run(command, args, options = {}) {
  const result = spawnSync(command, args, {
    cwd: options.cwd || work,
    encoding: "utf8",
    timeout: options.timeout || 60000,
    env: options.env || cleanEnv(),
  });
  if (result.status !== 0 && !options.allowFail) {
    throw new Error(`${command} ${args.join(" ")} -> ${result.status}\n${result.stderr || result.stdout}`);
  }
  return result;
}

function cleanEnv(extra = {}) {
  return {
    PATH: "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin",
    HOME: work,
    LANG: "C.UTF-8",
    GIT_TERMINAL_PROMPT: "0",
    GIT_AUTHOR_NAME: "Fixture",
    GIT_AUTHOR_EMAIL: "fixture@example.test",
    GIT_COMMITTER_NAME: "Fixture",
    GIT_COMMITTER_EMAIL: "fixture@example.test",
    NO_PROXY: "*",
    no_proxy: "*",
    ALL_PROXY: "",
    all_proxy: "",
    HTTP_PROXY: "",
    http_proxy: "",
    HTTPS_PROXY: "",
    https_proxy: "",
    ...extra,
  };
}

function record(name, ok, text) {
  report.checks.push({ name, exit: ok ? 0 : 1, stdout: String(text).slice(0, 1500), stderr: "" });
  if (!ok) throw new Error(name + ": " + text);
}

async function api(path, method = "GET", data) {
  return new Promise((resolvePromise, reject) => {
    const request = http.request(base + path, {
      method,
      headers: { Authorization: "Bearer " + token, "Content-Type": "application/json" },
    }, (response) => {
      let text = "";
      response.on("data", (chunk) => { text += chunk; });
      response.on("end", () => {
        let body = text;
        try { body = text ? JSON.parse(text) : null; } catch { /* keep text */ }
        resolvePromise({ status: response.statusCode, body, text });
      });
    });
    request.setTimeout(8000, () => request.destroy(new Error("api timeout " + path)));
    request.on("error", reject);
    request.end(data === undefined ? undefined : JSON.stringify(data));
  });
}

async function gql(query, variables) {
  return api("/api/graphql", "POST", { query, variables });
}

async function start() {
  const binary = join(work, "gh-server");
  run("go", ["build", "-mod=readonly", "-o", binary, "./cmd/gh-server"], { cwd: root, timeout: 180000, env: { ...process.env, GOFLAGS: "-mod=readonly" } });
  writeFileSync(join(work, "integrations.yaml"), JSON.stringify({ ci: { default_backend: "native", backends: {}, repositories: {} } }));
  server = spawn(binary, [], {
    cwd: serverHome,
    detached: true,
    stdio: ["ignore", "pipe", "pipe"],
    env: {
      ...cleanEnv(),
      PORT: port,
      DB_DSN: "sqlite:" + join(serverHome, "ags.sqlite") + "?_foreign_keys=on",
      GIT_REPO_DIR: join(serverHome, "repos"),
      BASE_URL: base,
      AGS_API_BASE_URL: base,
      ADMIN_LOGIN: "fixture",
      ADMIN_TOKEN: token,
      LISTEN_MODE: "production",
      ENVIRONMENT: "development",
      ENABLE_WORKFLOW_EXEC: "0",
      AGS_INTEGRATIONS_CONFIG: join(work, "integrations.yaml"),
    },
  });
  const logFile = createWriteStream("/tmp/ags-parity2-live.log");
  for (const pipe of [server.stdout, server.stderr]) {
    pipe.on("data", (chunk) => { serverLog = (serverLog + chunk).slice(-20000); logFile.write(chunk); });
  }
  for (let i = 0; i < 200; i++) {
    if (server.exitCode !== null) throw new Error("server exited\n" + serverLog);
    try {
      const ready = await api("/readyz");
      if (ready.status === 200) return;
    } catch { /* retry */ }
    await new Promise((resolvePromise) => setTimeout(resolvePromise, 100));
  }
  throw new Error("server not ready\n" + serverLog);
}

function gitEnv() {
  return cleanEnv({
    GIT_CONFIG_COUNT: "1",
    GIT_CONFIG_KEY_0: "http.extraHeader",
    GIT_CONFIG_VALUE_0: "Authorization: Bearer " + token,
  });
}

async function main() {
  await start();
  const created = await api("/api/v3/user/repos", "POST", { name: "parity", private: false, auto_init: true, default_branch: "main" });
  record("create repo", created.status === 201, created.status + " " + created.text);
  run("git", ["clone", base + "/" + repo + ".git", checkout], { env: gitEnv() });
  run("git", ["switch", "-c", "feature"], { cwd: checkout, env: gitEnv() });
  writeFileSync(join(checkout, "feature.txt"), "feature\n");
  run("git", ["add", "feature.txt"], { cwd: checkout, env: gitEnv() });
  run("git", ["commit", "-m", "feature"], { cwd: checkout, env: gitEnv() });
  run("git", ["push", "-u", "origin", "feature"], { cwd: checkout, env: gitEnv() });
  const head = run("git", ["rev-parse", "HEAD"], { cwd: checkout, env: gitEnv() }).stdout.trim();
  const opened = await api("/api/v3/repos/" + repo + "/pulls", "POST", { title: "parity", head: "feature", base: "main", body: "body" });
  record("create pr", opened.status === 201, opened.status + " " + opened.text.slice(0, 300));

  writeFileSync(join(work, "body.md"), "edited from file\n");
  const edited = await api("/api/v3/repos/" + repo + "/pulls/1", "PATCH", { body: readFileSync(join(work, "body.md"), "utf8") });
  record("edit body", edited.status === 200 && edited.body.body === "edited from file\n", JSON.stringify(edited.body && edited.body.body));
  writeFileSync(join(work, "comment.md"), "comment from file\n");
  const commented = await api("/api/v3/repos/" + repo + "/issues/1/comments", "POST", { body: readFileSync(join(work, "comment.md"), "utf8") });
  record("comment body", commented.status === 201 && commented.body.body === "comment from file\n", commented.status + " " + JSON.stringify(commented.body && commented.body.body));

  const checks = await api(`/api/v3/repos/${repo}/commits/${head}/check-runs`);
  record("check-runs empty", checks.status === 200 && checks.body.total_count === 0 && Array.isArray(checks.body.check_runs), JSON.stringify(checks.body));
  const artifacts = await api(`/api/v3/repos/${repo}/actions/artifacts`);
  record("native artifacts", artifacts.status === 200, artifacts.status + " " + artifacts.text.slice(0, 200));
  const runs = await api(`/api/v3/repos/${repo}/actions/runs`);
  record("run list", runs.status === 200 && Array.isArray(runs.body.workflow_runs), runs.status + " " + runs.text.slice(0, 200));

  run("git", ["switch", "main"], { cwd: checkout, env: gitEnv() });
  writeFileSync(join(checkout, "base.txt"), "advance\n");
  run("git", ["add", "base.txt"], { cwd: checkout, env: gitEnv() });
  run("git", ["commit", "-m", "advance base"], { cwd: checkout, env: gitEnv() });
  run("git", ["push", "origin", "main"], { cwd: checkout, env: gitEnv() });
  const behind = await gql("query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){mergeStateStatus state}}}", { owner: "fixture", name: "parity", number: 1 });
  const behindText = JSON.stringify(behind.body);
  const behindPR = behind.body && behind.body.data && behind.body.data.repository && behind.body.data.repository.pullRequest;
  record("mergeStateStatus BEHIND", behind.status === 200 && behindPR && behindPR.mergeStateStatus === "BEHIND", behindText.slice(0, 800));

  const disabled = await api("/api/v3/repos/" + repo, "PATCH", { allow_squash_merge: false });
  record("disable squash", disabled.status === 200 && disabled.body.allow_squash_merge === false, disabled.status + " " + JSON.stringify(disabled.body && disabled.body.allow_squash_merge));
  const squash = await api("/api/v3/repos/" + repo + "/pulls/1/merge", "PUT", { merge_method: "squash", sha: head });
  record("squash 409", squash.status === 409, squash.status + " " + squash.text.slice(0, 300));
  const merged = await api("/api/v3/repos/" + repo + "/pulls/1/merge", "PUT", { merge_method: "merge", sha: head });
  record("merge commit lands", merged.status === 200 && merged.body.merged === true && merged.body.sha && merged.body.sha !== head, merged.status + " " + merged.text.slice(0, 300));
  const viewed = await gql("query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){state mergeCommit{oid}}}}", { owner: "fixture", name: "parity", number: 1 });
  const viewedPR = viewed.body && viewed.body.data && viewed.body.data.repository && viewed.body.data.repository.pullRequest;
  record("MERGED readback", viewedPR && viewedPR.state === "MERGED" && viewedPR.mergeCommit && viewedPR.mergeCommit.oid === merged.body.sha, JSON.stringify(viewed.body).slice(0, 500));

  await maybeOfficialGh(head);
  console.log(JSON.stringify(report, null, 2));
}

async function maybeOfficialGh(head) {
  if (process.env.AGS_E2E_GH !== "1") {
    report.officialGh = "skipped; stock gh requires HTTPS for a :port host. Set AGS_E2E_GH=1 on a host where the TLS front completes.";
    return;
  }
  run("openssl", ["req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", join(work, "key.pem"), "-out", join(work, "cert.pem"), "-days", "1", "-subj", "/CN=127.0.0.1", "-addext", "subjectAltName=IP:127.0.0.1"]);
  tlsProxy = https.createServer({ key: readFileSync(join(work, "key.pem")), cert: readFileSync(join(work, "cert.pem")) }, (request, response) => {
    const headers = { ...request.headers, host: "127.0.0.1:" + port };
    delete headers.connection;
    const upstream = http.request(base + request.url, { method: request.method, headers }, (upstreamResponse) => {
      response.writeHead(upstreamResponse.statusCode, upstreamResponse.headers);
      upstreamResponse.pipe(response);
    });
    upstream.on("error", () => { if (!response.headersSent) { response.writeHead(502); response.end(); } });
    request.pipe(upstream);
  });
  await new Promise((resolvePromise) => tlsProxy.listen(Number(tlsPort), "127.0.0.1", resolvePromise));
  const ghHome = join(work, "gh");
  mkdirSync(ghHome, { mode: 0o700 });
  writeFileSync(join(ghHome, "config.yml"), "telemetry: disabled\nprompt: disabled\n");
  writeFileSync(join(ghHome, "hosts.yml"), `127.0.0.1:\n  user: fixture\n  oauth_token: ${JSON.stringify(token)}\n  git_protocol: https\n`);
  const result = run(ghBin, ["pr", "view", "1", "--repo", repo, "--json", "state,mergeStateStatus"], {
    cwd: checkout,
    env: cleanEnv({
      GH_CONFIG_DIR: ghHome,
      GH_HOST: "127.0.0.1:" + tlsPort,
      GH_ENTERPRISE_TOKEN: token,
      SSL_CERT_FILE: join(work, "cert.pem"),
    }),
    allowFail: true,
  });
  record("official gh pr view", result.status === 0, (result.stdout || "") + (result.stderr || ""));
}

main().catch((error) => {
  report.error = String(error.message || error);
  report.serverLog = serverLog.slice(-4000);
  console.log(JSON.stringify(report, null, 2));
  process.exitCode = 1;
}).finally(() => {
  if (tlsProxy) tlsProxy.close();
  if (server && server.exitCode === null) {
    try { process.kill(-server.pid, "SIGTERM"); } catch { /* already gone */ }
  }
  if (process.env.AGS_E2E_KEEP !== "1") rmSync(work, { recursive: true, force: true });
});
