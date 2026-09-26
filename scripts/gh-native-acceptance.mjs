#!/usr/bin/env node
/** Opt-in, real official gh against this checkout. No production configuration,
 * auth, network, or databases are inherited. Each run owns disposable artifacts.
 */
import { spawnSync } from "node:child_process";
import { mkdtempSync, mkdirSync, readFileSync, writeFileSync, rmSync } from "node:fs";
import { createHash } from "node:crypto";
import { tmpdir } from "node:os";
import { resolve, dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const companionArg = process.argv.indexOf("--companion");
const companion = companionArg >= 0 ? resolve(process.argv[companionArg + 1] ?? "") : undefined;
if (companion) {
  const manifest=JSON.parse(readFileSync(join(companion,"build-manifest.json"),"utf8"));
  if (manifest.schema!=="agsx.build.v1"||!manifest.files?.["dist/cli.js"]||manifest.files?.["dist/git-shim.js"]) throw new Error("Expected a built no-shim companion candidate.");
}
if (process.platform !== "linux" || process.arch !== "x64") throw new Error("Linux x64 namespace fixture required.");
function run(command, args, options = {}) {
  const p = spawnSync(command, args, { cwd: root, encoding: "utf8", maxBuffer: 16 << 20, timeout: 180000, ...options });
  if (p.status !== 0) throw new Error(`${command} exited ${p.status}: ${(p.stderr || "").slice(-1500)}`);
  return p.stdout;
}
// Ubuntu hosted runners can restrict unprivileged user namespaces. The
// explicit CI flag elevates namespace creation only. Map namespace root back
// to the original host UID/GID so the worker can read its owner's private build
// artifacts without host-root access or widening filesystem permissions.
const namespace = process.argv.includes("--privileged-namespace")
  ? ["sudo", "-n", "unshare", "--user", `--map-users=0:${process.getuid()}:1`, `--map-groups=0:${process.getgid()}:1`, "--setuid=0", "--setgid=0", "--net"]
  : ["unshare", "--user", "--map-root-user", "--net"];
run(namespace[0], [...namespace.slice(1), "true"]);
const temp = mkdtempSync(join(tmpdir(), "ags-stock-gh-"));
const ghFixture = { version: "2.101.0", sha256: "9bca2d1c16825f109907a23307628a2f0698fbf99662b73a5cf0b020293072b8", url: "https://github.com/cli/cli/releases/download/v2.101.0/gh_2.101.0_linux_amd64.tar.gz" };
try {
  const archive = join(temp, "gh.tgz");
  run("curl", ["--fail", "--location", "--silent", "--show-error", "--proto", "=https", "--proto-redir", "=https", "--max-time", "90", "--max-filesize", "33554432", ghFixture.url, "-o", archive]);
  if (createHash("sha256").update(readFileSync(archive)).digest("hex") !== ghFixture.sha256) throw new Error("Official gh asset hash mismatch.");
  const ghPath = "gh_2.101.0_linux_amd64/bin/gh";
  run("tar", ["-xzf", archive, "-C", temp, ghPath]);
  const source = run("git", ["rev-parse", "HEAD"]).trim();
  const dirty = run("git", ["status", "--porcelain"]).trim() !== "";
  const binary = join(temp, "gh-server");
  run("go", ["build", "-p", "2", "-mod=readonly", "-trimpath", "-ldflags=-X github.com/ngaut/agent-git-service/server.gitSHA=" + source, "-o", binary, "./cmd/gh-server"], { env: { ...process.env, GOMAXPROCS: "3" } });
  const result = run(namespace[0], [...namespace.slice(1), process.execPath, join(root, "scripts/gh-native-worker.mjs"), binary, join(temp, ghPath), ...(companion ? [companion] : [])], { timeout: 240000 });
  const receipt = { ...JSON.parse(result), source: { commit: source, dirty }, ghFixture };
  mkdirSync(join(root, ".test-build"), { recursive: true });
  writeFileSync(join(root, ".test-build/gh-native.json"), JSON.stringify(receipt, null, 2) + "\n", { mode: 0o600 });
  console.log(JSON.stringify(receipt));
  if (!receipt.passed) process.exitCode = 1;
} finally { rmSync(temp, { recursive: true, force: true }); }
