#!/usr/bin/env node
/** Child of gh-native-acceptance: every effect stays in a fresh loopback netns. */
import { spawn, spawnSync } from "node:child_process";
import { mkdtempSync, mkdirSync, readFileSync, writeFileSync, rmSync } from "node:fs";
import { randomBytes } from "node:crypto";
import { tmpdir, networkInterfaces } from "node:os";
import { join } from "node:path";
import http from "node:http";
import https from "node:https";
const [binary, gh, companion] = process.argv.slice(2);
if (!binary || !gh || process.getuid() !== 0 || spawnSync("ip", ["link", "set", "lo", "up"]).status !== 0 || Object.keys(networkInterfaces()).some(x => x !== "lo")) throw new Error("A private loopback-only namespace is required.");
const work = mkdtempSync(join(tmpdir(), "ags-native-gh-worker-"));
const serverHome = join(work, "server"), ghHome = join(work, "gh"), checkout = join(work, "project");
for (const p of [serverHome, ghHome]) mkdirSync(p, { mode: 0o700 });
const token = randomBytes(32).toString("hex"), providerToken = randomBytes(32).toString("hex"), bridgeToken = randomBytes(32).toString("hex");
const secrets = [token, providerToken, bridgeToken];
const clean = value => secrets.reduce((s, secret) => s.split(secret).join("[REDACTED]"), String(value));
const env = { PATH: "/usr/local/bin:/usr/bin:/bin", HOME: work, LANG: "C.UTF-8", NO_PROXY: "*", GH_CONFIG_DIR: ghHome, GH_PROMPT_DISABLED: "1", GH_NO_UPDATE_NOTIFIER: "1", GH_NO_EXTENSION_UPDATE_NOTIFIER: "1", GIT_TERMINAL_PROMPT: "0", GIT_CONFIG_NOSYSTEM: "1", GIT_CONFIG_GLOBAL: "/dev/null", GIT_AUTHOR_NAME: "Fixture", GIT_AUTHOR_EMAIL: "fixture@example.test", GIT_COMMITTER_NAME: "Fixture", GIT_COMMITTER_EMAIL: "fixture@example.test", SSL_CERT_FILE: join(work, "cert.pem") };
const repo = "fixture/gh-native", base = "http://127.0.0.1:6666";
const report = { schema: "ags.official-gh-acceptance.v1", network: "isolated loopback-only user+network namespace", productionData: false, productionCredentials: false, shim: false, routingOverrides: false, checks: [], startedAt: new Date().toISOString() };
let primary, front, provider, stage = "start", serverLog = "", head = "a".repeat(40), outcome = "failure", currentBackend = "native", effects = 0;
const assertions = (value, message) => { if (!value) throw new Error(message); };
async function cmd(executable, args, cwd = work, extra = {}) {
  return new Promise((resolve, reject) => {
    const child = spawn(executable, args, { cwd, env: { ...env, ...extra }, stdio: ["ignore", "pipe", "pipe"] });
    let out = "", err = "";
    child.stdout.on("data", b => { out += b; if (out.length > 4 << 20) child.kill("SIGKILL"); });
    child.stderr.on("data", b => { err += b; if (err.length > 4 << 20) child.kill("SIGKILL"); });
    const timer = setTimeout(() => child.kill("SIGKILL"), 25000);
    child.on("error", e => { clearTimeout(timer); reject(e); });
    child.on("close", code => { clearTimeout(timer); resolve({ code, stdout: clean(out.trim()), stderr: clean(err.trim()) }); });
  });
}
async function api(path, method = "GET", data, credential = token) {
  return new Promise((resolve, reject) => {
    const request = http.request(base + path, { method, headers: { Authorization: "Bearer " + credential, "Content-Type": "application/json" } }, response => {
      let text = ""; response.on("data", b => text += b); response.on("end", () => { try { resolve({ status: response.statusCode, data: text ? JSON.parse(text) : null }); } catch { reject(new Error("invalid JSON at " + path)); } });
    });
    request.setTimeout(8000, () => request.destroy(new Error("fixture API deadline"))); request.on("error", reject); request.end(data === undefined ? undefined : JSON.stringify(data));
  });
}
async function ghCheck(name, args, accepted = [0], extra = {}) {
  stage = name; const result = await cmd(gh, args, checkout, extra);
  report.checks.push({ name, exit: result.code, stdout: result.stdout.slice(0, 1000), stderr: result.stderr.slice(0, 1000), backend: currentBackend });
  assertions(accepted.includes(result.code), `${name}: ${result.code}: ${result.stderr}`); return result;
}
async function git(args, cwd = checkout) {
  const r = await cmd("git", ["-c", "http.sslCAInfo=" + join(work, "cert.pem"), "-c", "http.https://127.0.0.1/.extraHeader=Authorization: Bearer " + token, ...args], cwd);
  assertions(r.code === 0, "fixture Git: " + r.stderr); return r.stdout;
}
async function stop() {
  if (primary && primary.exitCode === null) {
    const old = primary; try { process.kill(-old.pid, "SIGTERM"); } catch {}
    await new Promise(resolve => { const timer = setTimeout(() => { try { process.kill(-old.pid, "SIGKILL"); } catch {} resolve(); }, 6000); old.once("close", () => { clearTimeout(timer); resolve(); }); });
  }
}
async function start(backend) {
  await stop(); currentBackend = backend;
  const required = ["unit", "lint"];
  const ci = { default_backend: "native", backends: { github: { kind: "github-actions", url: "http://127.0.0.1:7000", allow_http: true, token_file: join(work, "provider.token") }, forgejo: { kind: "forgejo", url: "http://127.0.0.1:7000", allow_http: true, token_file: join(work, "provider.token"), log_bridge: { url: "http://127.0.0.1:7000", allow_http: true, token_file: join(work, "bridge.token") } } }, repositories: { [repo]: { backend, ...(backend === "native" || backend === "none" ? {} : { repository: "ci/project", required_checks: required }) } } };
  writeFileSync(join(work, "integrations.yaml"), JSON.stringify({ ci }), { mode: 0o600 });
  primary = spawn(binary, [], { cwd: serverHome, env: { ...env, PORT: "6666", DB_DSN: "sqlite:" + join(serverHome, "ags.sqlite") + "?_foreign_keys=on", GIT_REPO_DIR: join(serverHome, "repos"), BASE_URL: "http://127.0.0.2:6666", AGS_API_BASE_URL: "https://127.0.0.1", ADMIN_LOGIN: "fixture", ADMIN_TOKEN: token, LISTEN_MODE: "production", ENVIRONMENT: "development", ENABLE_WORKFLOW_EXEC: "0", AGS_INTEGRATIONS_CONFIG: join(work, "integrations.yaml") }, stdio: ["ignore", "pipe", "pipe"], detached: true });
  for (const pipe of [primary.stdout, primary.stderr]) pipe.on("data", b => serverLog = (serverLog + b).slice(-6000));
  for (let n = 0; n < 200; n++) { if (primary.exitCode !== null) throw new Error("server exited: " + clean(serverLog)); try { const r = await api("/readyz"); if (r.status === 200) return; } catch {} await new Promise(r => setTimeout(r, 100)); }
  throw new Error("server readiness deadline");
}
function json(response, body, status = 200) { response.writeHead(status, { "Content-Type": "application/json" }); response.end(JSON.stringify(body)); }
function runObject() {
  const now = "2026-01-01T00:00:00Z", done = outcome !== "pending";
  if (currentBackend === "forgejo") return { id: 17, index_in_repo: 9, workflow_id: "ci.yml", title: "fixture commit", prettyref: "feature", commit_sha: head, event: "push", status: done ? outcome : "running", html_url: "http://127.0.0.1:7000/ci/project/actions/runs/9", created: now, updated: now, started: now, stopped: done ? now : null };
  return { id: 17, name: "CI", workflow_id: 5, head_branch: "feature", head_sha: head, event: "push", status: done ? "completed" : "in_progress", conclusion: done ? outcome : null, html_url: "http://127.0.0.1:7000/ci/project/actions/runs/17", run_number: 9, run_attempt: 1, created_at: now, updated_at: now, run_started_at: now };
}
function jobs() {
  return ["unit", "lint"].map((name, index) => ({ id: 41 + index, run_id: 17, name, head_sha: head, status: outcome === "pending" ? "in_progress" : "completed", conclusion: outcome === "pending" ? null : index === 0 ? outcome : "success", html_url: `http://127.0.0.1:7000/ci/project/actions/runs/17/job/${index}`, started_at: "2026-01-01T00:00:00Z", completed_at: outcome === "pending" ? null : "2026-01-01T00:01:00Z", steps: [] }));
}
function zip(files) {
  // Stored ZIP fixture: independently shaped logs, not a mock of service code.
  let offset = 0; const blocks = [], directory = [];
  const crc = data => { let n = 0xffffffff; for (const byte of data) { n ^= byte; for (let i = 0; i < 8; i++) n = (n >>> 1) ^ ((n & 1) ? 0xedb88320 : 0); } return (n ^ 0xffffffff) >>> 0; };
  for (const [name, text] of files) {
    const path = Buffer.from(name), data = Buffer.from(text), checksum = crc(data);
    const header = Buffer.alloc(30); header.writeUInt32LE(0x04034b50); header.writeUInt16LE(20, 4); header.writeUInt32LE(checksum, 14); header.writeUInt32LE(data.length, 18); header.writeUInt32LE(data.length, 22); header.writeUInt16LE(path.length, 26);
    const central = Buffer.alloc(46); central.writeUInt32LE(0x02014b50); central.writeUInt16LE(20, 4); central.writeUInt16LE(20, 6); central.writeUInt32LE(checksum, 16); central.writeUInt32LE(data.length, 20); central.writeUInt32LE(data.length, 24); central.writeUInt16LE(path.length, 28); central.writeUInt32LE(offset, 42);
    blocks.push(header, path, data); directory.push(central, path); offset += header.length + path.length + data.length;
  }
  const dir = Buffer.concat(directory), end = Buffer.alloc(22); end.writeUInt32LE(0x06054b50); end.writeUInt16LE(files.length, 8); end.writeUInt16LE(files.length, 10); end.writeUInt32LE(dir.length, 12); end.writeUInt32LE(offset, 16); return Buffer.concat([...blocks, dir, end]);
}
try {
  const cert = await cmd("openssl", ["req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", join(work, "key.pem"), "-out", join(work, "cert.pem"), "-days", "1", "-subj", "/CN=127.0.0.1", "-addext", "subjectAltName=IP:127.0.0.1"]); assertions(cert.code === 0, "TLS fixture failed");
  writeFileSync(join(ghHome, "config.yml"), "telemetry: disabled\nprompt: disabled\n", { mode: 0o600 });
  writeFileSync(join(ghHome, "hosts.yml"), `127.0.0.1:\n  user: fixture\n  oauth_token: ${JSON.stringify(token)}\n  git_protocol: https\n`, { mode: 0o600 });
  writeFileSync(join(work, "provider.token"), providerToken, { mode: 0o600 }); writeFileSync(join(work, "bridge.token"), bridgeToken, { mode: 0o600 });
  front = https.createServer({ key: readFileSync(join(work, "key.pem")), cert: readFileSync(join(work, "cert.pem")) }, (request, response) => { const upstream = http.request(base + request.url, { method: request.method, headers: request.headers }, r => { response.writeHead(r.statusCode, r.headers); r.pipe(response); }); upstream.on("error", () => { response.writeHead(502); response.end(); }); request.pipe(upstream); });
  await new Promise(r => front.listen(443, "127.0.0.1", r));
  provider = http.createServer((req, res) => {
    const url = new URL(req.url, "http://127.0.0.1:7000"), bridge = url.pathname.startsWith("/api/internal/provider-logs/");
    const auth = bridge ? "Bearer " + bridgeToken : (currentBackend === "forgejo" ? "token " : "Bearer ") + providerToken;
    if (req.headers.authorization !== auth) { json(res, { error: "wrong credential boundary" }, 401); return; }
    if (bridge) {
      const task = Number(url.pathname.split("/").at(-1)); const name = task === 41 ? "unit" : "lint";
      if (url.searchParams.get("head_sha") !== head || url.searchParams.get("head_ref") !== "feature") { json(res, {}, 409); return; }
      json(res, { schema: "ags.internal-provider-log.v1", repo: "ci/project", task_id: task, run_number: 9, job_name: name, head_sha: head, provider_pr: 0, provider_ref: "refs/heads/feature", event: "push", text: `2026-01-01T00:00:00Z ${name} fixture log\n` }); return;
    }
    const path = url.pathname.replace(/^\/api\/v1/, "");
    if (req.method === "POST") { if (currentBackend !== "github" || !/\/runs\/17\/(cancel|rerun|rerun-failed-jobs)$/.test(path)) { json(res, {}, 405); return; } effects++; res.writeHead(202); res.end(); return; }
    if (path.endsWith("/actions/runs")) { json(res, { total_count: 1, workflow_runs: [runObject()] }); return; }
    if (path.endsWith("/actions/runs/17")) { json(res, runObject()); return; }
    if (path.endsWith("/actions/tasks")) { json(res, { total_count: 2, workflow_runs: jobs().map(j => ({ id: j.id, run_number: 9, workflow_id: "ci.yml", name: j.name, head_sha: head, head_branch: "feature", status: j.status === "in_progress" ? "running" : j.conclusion, url: j.html_url, created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:01:00Z", run_started_at: j.started_at })) }); return; }
    if (path.endsWith("/actions/runs/17/jobs")) { json(res, { total_count: 2, jobs: jobs() }); return; }
    if (/\/actions\/jobs\/(41|42)$/.test(path)) { json(res, jobs().find(j => j.id === Number(path.split("/").at(-1)))); return; }
    if (/\/actions\/jobs\/(41|42)\/logs$/.test(path)) { res.writeHead(200, { "Content-Type": "text/plain" }); res.end("2026-01-01T00:00:00Z exact job fixture log\n"); return; }
    if (path.endsWith("/actions/runs/17/logs")) { res.writeHead(200, { "Content-Type": "application/zip" }); res.end(zip([["0_unit.txt", "2026-01-01T00:00:00Z unit fixture log\n"], ["1_lint.txt", "2026-01-01T00:00:00Z lint fixture log\n"]])); return; }
    if (path.endsWith("/actions/workflows")) { json(res, {total_count: 1, workflows: [{id: 5, name: "CI", path: ".github/workflows/ci.yml", state: "active", created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z"}]}); return; }
    if (path.endsWith("/actions/workflows/5")) { json(res, { id: 5, name: "CI", path: ".github/workflows/ci.yml", state: "active", created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z" }); return; }
    json(res, { path, unsupportedFixture: true }, 404);
  }); await new Promise(r => provider.listen(7000, "127.0.0.1", r));
  await start("native");
  let response = await api("/api/v3/user/repos", "POST", { name: "gh-native", private: true, auto_init: true, default_branch: "main" }); assertions(response.status === 201, "native repository creation failed");
  await git(["clone", "https://127.0.0.1/" + repo + ".git", checkout], work); await git(["switch", "-c", "feature"]); writeFileSync(join(checkout, "feature.txt"), "fixture\n"); await git(["add", "feature.txt"]); await git(["commit", "-m", "feature"]); await git(["push", "-u", "origin", "feature"]); head = await git(["rev-parse", "HEAD"]);
  await ghCheck("origin-only repository", ["repo", "view", "--json", "nameWithOwner,url"]);
  await ghCheck("HEAD discovery", ["browse", "--no-browser"]);
  await ghCheck("draft create", ["pr", "create", "--base", "main", "--title", "Official gh fixture", "--body", "isolated", "--draft"]);
  response = await api("/api/v3/repos/" + repo + "/pulls/1"); assertions(response.status === 200 && response.data.draft === true, "draft flag silently ignored");
  await ghCheck("ready for review", ["pr", "ready", "1"]);
  const noCI = await ghCheck("no CI still has commit", ["pr", "checks", "1"], [1]); assertions(!noCI.stderr.includes("no commit found"), "missing CI removed commit shape");
  await start("github");
  await ghCheck("GitHub Actions failure", ["pr", "checks", "1", "--required"], [1]);
  const listed = await ghCheck("GitHub Actions runs", ["run", "list", "--json", "databaseId,workflowName,status,conclusion,headSha"]); const githubRun = JSON.parse(listed.stdout)[0].databaseId; assertions(githubRun > 2 ** 52, "provider ID was not mapped");
  const logs = await ghCheck("GitHub Actions failed logs", ["run", "view", String(githubRun), "--log-failed"]); assertions(logs.stdout.includes("unit fixture log"), "lost failure logs");
  await ghCheck("GitHub Actions cancel", ["run", "cancel", String(githubRun)]); assertions(effects === 1, "CI mutation was repeated or not sent");
  outcome = "pending"; await ghCheck("GitHub Actions pending", ["pr", "checks", "1", "--required"], [8]);
  outcome = "success"; await ghCheck("GitHub Actions success", ["pr", "checks", "1", "--required"]);
  await start("forgejo");
  await ghCheck("Forgejo required checks", ["pr", "checks", "1", "--required"]);
  const forgejoListed = await ghCheck("Forgejo native run shape", ["run", "list", "--json", "databaseId,workflowName,status,conclusion,headSha"]); const forgejoRun = JSON.parse(forgejoListed.stdout)[0].databaseId; assertions(forgejoRun !== githubRun, "backend switch aliased a historical ID");
  response = await api(`/api/v3/repos/${repo}/actions/runs/${githubRun}`); assertions(response.status === 404, "old backend run remains addressable as new backend");
  const forgejoLogs = await ghCheck("Forgejo exact bridge logs", ["run", "view", String(forgejoRun), "--log"]); assertions(forgejoLogs.stdout.includes("unit fixture log") && forgejoLogs.stdout.includes("lint fixture log"), "Forgejo job logs missing");
  await ghCheck("Forgejo unsupported cancel", ["run", "cancel", String(forgejoRun)], [1]); assertions(effects === 1, "unsupported backend mutation fell back");
  const before = await git(["rev-parse", "origin/main"]);
  await ghCheck("wrong head merge", ["pr", "merge", "1", "--merge", "--match-head-commit", "b".repeat(40)], [1]);
  response = await api(`/api/v3/repos/${repo}/branches/main`); assertions(response.data.commit.sha === before, "rejected merge changed destination");
  await ghCheck("standard merge after external checks", ["pr", "merge", "1", "--merge", "--match-head-commit", head]);
  response = await api(`/api/v3/repos/${repo}/pulls/1`); assertions(response.status === 200 && response.data.merged, "merge exit success without native merged fact");
  // Separate task run credentials use the same native actor. Source metadata is
  // explicitly provisional here; it grants no identity or repository permission.
  const one = await api("/api/ext/v1/client-runs", "POST", { context: { source: "fixture", agent: "fixture-actor", task: "task-one", run: "run-one" } });
  const two = await api("/api/ext/v1/client-runs", "POST", { context: {} }); assertions(one.status === 201 && two.status === 201 && one.data.id !== two.data.id, "run identity setup failed"); secrets.push(one.data.token, two.data.token);
  assertions(one.data.association_status === "provisional" && two.data.association_status === "unlinked", "unverified context claimed linked");
  const runHome = join(work, "run-gh"); mkdirSync(runHome, { mode: 0o700 }); writeFileSync(join(runHome, "hosts.yml"), `127.0.0.1:\n  user: fixture\n  oauth_token: ${JSON.stringify(one.data.token)}\n  git_protocol: https\n`, { mode: 0o600 });
  await git(["switch", "-c", "run-work"]); writeFileSync(join(checkout, "run.txt"), "run fixture\n"); await git(["add", "run.txt"]); await git(["commit", "-m", "run provenance"]); await git(["push", "-u", "origin", "run-work"]);
  await ghCheck("ordinary gh with task identity", ["pr", "create", "--base", "main", "--title", "Run provenance", "--body", "no operation grants"], [0], { GH_CONFIG_DIR: runHome });
  response = await api(`/api/ext/v1/repos/${repo}/pulls/2/context`); assertions(response.status === 200 && response.data.runs[0].id === one.data.id && response.data.runs[0].context.task === "task-one", "run provenance did not follow PR creation");
  response = await api("/api/ext/v1/client-runs/" + one.data.id, "DELETE", undefined, one.data.token); assertions(response.status === 204, "run revoke failed");
  await ghCheck("revoked task credential rejected", ["repo", "view", "--json", "nameWithOwner"], [1], { GH_CONFIG_DIR: runHome });
  response = await api("/api/v3/user", "GET", undefined, two.data.token); assertions(response.status === 200, "revocation crossed task boundary");
  if (companion) {
    async function companionCall(name, input) {
      stage = "companion " + name;
      const args = [join(companion,"dist/cli.js"),name];
      for (const [key,value] of Object.entries(input)) args.push("--"+key.replace(/[A-Z]/g,c=>"-"+c.toLowerCase()),typeof value==="object"?JSON.stringify(value):String(value));
      const result=await cmd(process.execPath,args,checkout,{AGSX_HOME:join(work,"companion-home")});
      let parsed;try{parsed=JSON.parse(result.stdout);}catch{throw new Error("Companion did not emit its JSON envelope");}
      assertions(result.code===0,stage+": "+result.code+" "+JSON.stringify(parsed.error??{}));
      report.checks.push({name:stage,exit:result.code,status:parsed.status});return parsed.data;
    }
    const parentFile=join(work,"parent.token");writeFileSync(parentFile,token,{mode:0o600});
    const config=join(work,"companion-gh");mkdirSync(config,{mode:0o700});
    writeFileSync(join(config,"hosts.yml"),"github.com:\n  user: isolated-existing-account\n  oauth_token: synthetic-unrelated-token\n",{mode:0o600});
    const common={gh,ghConfig:config,caFile:join(work,"cert.pem")};
    const plan=await companionCall("setup.plan",{...common,host:"127.0.0.1",tokenFile:parentFile});
    await companionCall("setup.apply",{...common,host:"127.0.0.1",tokenFile:parentFile,expectedPlan:plan.expectedPlan,apply:true});
    assertions(readFileSync(join(config,"hosts.yml"),"utf8").includes("synthetic-unrelated-token"),"setup replaced another host's credential");
    await companionCall("doctor",{...common,network:true});
    await companionCall("ci.backend",common);
    // Start from the existing HTTP Git origin. API still uses hostname/HTTPS;
    // generated helper config must bind BOTH independently without a git shim.
    await git(["remote","set-url","origin",base+"/"+repo+".git"]);
    const sessionDir=join(work,"companion-task");
    const prepared=await companionCall("session.start",{...common,directory:sessionDir,context:{source:"fixture",agent:"native-agent",task:"companion-task",run:"companion-run"},apply:true});
    const environment=JSON.parse(readFileSync(prepared.environmentFile,"utf8"));
    assertions(!environment.set.PATH&&!environment.set.GH_HOST&&!environment.set.GH_REPO&&!environment.ghIsWrapped,"companion installed a per-command routing override");
    const taskEnv={...env,...environment.set};for(const key of environment.unset) delete taskEnv[key];
    // Explicit command helper above merges defaults, so pass undefined for
    // removed keys rather than inadvertently inheriting them in this fixture.
    for(const key of environment.unset)taskEnv[key]=undefined;
    await git(["switch","-c","companion-work"]);writeFileSync(join(checkout,"companion.txt"),"companion environment fixture\n");await git(["add","companion.txt"]);await git(["commit","-m","companion identity"]);
    const pushed=await cmd("git",["push","-u","origin","companion-work"],checkout,{...taskEnv,GIT_TRACE:"1"});
    if(pushed.code!==0){
      const saved=JSON.parse(readFileSync(join(sessionDir,"session.json"),"utf8"));
      const direct=spawnSync(process.execPath,[join(companion,"dist/credential-helper.js"),sessionDir,"get"],{cwd:checkout,env:taskEnv,input:"protocol=http\nhost=127.0.0.1:6666\npath="+repo+".git\n\n",encoding:"utf8"});
      const configuration=await cmd("git",["config","--get-urlmatch","credential",base+"/"+repo+".git"],checkout,taskEnv);
      report.companionFailure={helperDirect:{exit:direct.status,returnedCredential:direct.stdout.includes("password=ags_run_")},config:configuration,gitScopes:saved.gitScopes,environmentKeys:Object.keys(taskEnv).filter(k=>k.startsWith("GIT_CONFIG")||k==="GH_CONFIG_DIR"),credentialMappings:Object.fromEntries(Object.entries(environment.set).filter(([key])=>key.startsWith("GIT_CONFIG_KEY")))};
    }
    assertions(pushed.code===0,"ordinary Git with generated credential helper failed: "+pushed.stderr);
    await ghCheck("companion environment ordinary gh create",["pr","create","--base","main","--title","Companion identity","--body","official gh; no shim"],[0],taskEnv);
    const linked=await companionCall("association.show",{...common,number:3});
    assertions(linked.runs.length===1&&linked.runs[0].id===prepared.id&&linked.runs[0].context.task==="companion-task","companion created PR with wrong run context");
    await companionCall("session.show",{directory:sessionDir});
    await companionCall("session.end",{directory:sessionDir,caFile:join(work,"cert.pem"),apply:true});
    const ended=JSON.parse(readFileSync(join(sessionDir,"session.json"),"utf8"));assertions(ended.phase==="ended","companion did not clean its session");
    // A different canonical Git hostname may use the explicit official api_host
    // setting. This preserves legacy HTTP origins without a client command shim.
    await git(["remote","set-url","origin","http://127.0.0.2:6666/"+repo+".git"]);
    const aliasPlan=await companionCall("setup.plan",{...common,host:"127.0.0.2",apiHost:"127.0.0.1",tokenFile:parentFile});
    await companionCall("setup.apply",{...common,host:"127.0.0.2",apiHost:"127.0.0.1",tokenFile:parentFile,expectedPlan:aliasPlan.expectedPlan,apply:true});
    await ghCheck("official api_host origin routing",["pr","view","3","--json","number,headRefOid"],[0],{GH_CONFIG_DIR:config});
    await companionCall("ci.backend",common);
    const aliasDir=join(work,"alias-task");
    const aliasSession=await companionCall("session.start",{...common,directory:aliasDir,context:{task:"api-host-task",run:"api-host-run"},apply:true});
    const aliasEnv=JSON.parse(readFileSync(aliasSession.environmentFile,"utf8"));const aliasTask={...env,...aliasEnv.set};for(const key of aliasEnv.unset)aliasTask[key]=undefined;
    const aliasRead=await cmd("git",["ls-remote","origin","refs/heads/main"],checkout,aliasTask);assertions(aliasRead.code===0,"Git origin with distinct official API host failed: "+aliasRead.stderr);
    await ghCheck("official api_host task identity",["pr","view","3","--json","number"],[0],aliasTask);
    await companionCall("session.end",{directory:aliasDir,caFile:join(work,"cert.pem"),apply:true});
    report.companionIntegrated={source:JSON.parse(readFileSync(join(companion,"build-manifest.json"),"utf8")).source,officialGitPush:true,officialGhCreate:true,perCommandRoutingOverrides:false,unrelatedHostPreserved:true,distinctApiHost:true};
  }
  report.passed = true; report.gh = (await cmd(gh, ["--version"])).stdout; report.runCredentialsSeparated = true; report.externalBackendSwitch = { githubRun, forgejoRun }; report.expectedHeadEnforced = true;
} catch (e) { report.passed = false; report.failure = { stage, message: clean(e.message), server: clean(serverLog).slice(-2000) }; }
finally {
  await stop();
  for (const server of [front, provider]) if (server) { server.closeAllConnections(); await new Promise(r => server.close(r)); }
  rmSync(work, { recursive: true, force: true }); report.temporaryDataRemoved = true; report.finishedAt = new Date().toISOString();
}
console.log(JSON.stringify(report));
