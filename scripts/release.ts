#!/usr/bin/env -S bun
/**
 * Maintainer release entry for the downstream AGS fork.
 *
 * The local script decides and pushes one exact immutable tag. That tag is the
 * only publication trigger; GitHub Actions verifies, builds, attests and
 * publishes the Release.
 *
 * Usage:
 *   bun scripts/release.ts status
 *   bun scripts/release.ts rc [--dry-run] [--no-push] [--no-watch]
 *   bun scripts/release.ts stable [--from fork-YYYYMMDD.N-rcN] [--dry-run] [--no-push] [--no-watch]
 */
import { spawnSync } from "node:child_process";

const REPOSITORY = "yoyooyooo/agent-git-service";
const REPOSITORY_ID = 1384519373;
const UPSTREAM = "ngaut/agent-git-service";
const RELEASE_WORKFLOW = ".github/workflows/release.yml";
const CI_WORKFLOW = ".github/workflows/ci.yml";
const SECRET_WORKFLOW = ".github/workflows/secret-scan.yml";
const BRANCH = /^fork\/main\.(\d{8})$/;
const TAG = /^fork-(\d{8})\.([1-9]\d*)(?:-rc([1-9]\d*))?$/;

export type ParsedTag = { raw: string; date: string; serial: number; rc: number | null };
export type ReleaseLine = { serial: number; stable: string | null; rcs: ParsedTag[] };
type Command = "status" | "rc" | "stable";
type Options = { command: Command; dryRun: boolean; noPush: boolean; noWatch: boolean; from: string | null };
type Context = { branch: string; date: string; head: string };
type ReleasePlan = { kind: "rc" | "stable"; tag: string; sha: string; fromRc: string | null };

if (import.meta.main) {
  main().catch((error) => {
    console.error(error instanceof Error ? error.message : String(error));
    process.exitCode = 1;
  });
}

async function main(): Promise<void> {
  const options = parseArgs(process.argv.slice(2));
  const context = preflight();
  const tags = readReleaseTags();
  const lines = releaseLines(tags, context.date);

  if (options.command === "status") {
    console.log(JSON.stringify({
      repository: REPOSITORY,
      branch: context.branch,
      head: context.head,
      generation: context.date,
      latestLine: lines.at(-1) ?? null,
      nextRc: chooseNextRc(lines, context.date),
      promotableRc: choosePromotableRc(lines),
    }, null, 2));
    return;
  }

  const plan = options.command === "rc"
    ? planRc(context, lines)
    : planStable(context, lines, options.from);

  console.log(formatPlan(plan, options));
  verifyExactCI(plan.sha);
  ensureTagDrivenWorkflow(plan.sha);
  ensureTagAvailable(plan.tag);
  if (plan.fromRc !== null) verifyPromotableRc(plan.fromRc, plan.sha);

  if (options.dryRun) {
    console.log("Dry run complete. No tag was created or pushed.");
    return;
  }
  if (options.noPush) {
    createLocalTag(plan.tag, plan.sha);
    console.log(`${plan.tag} created locally at ${plan.sha}. No remote tag was pushed.`);
    return;
  }

  const previousRun = latestReleaseRunId();
  pushExactTag(plan.tag, plan.sha);
  createLocalTag(plan.tag, plan.sha);
  console.log(`${plan.tag} pushed. Tag-driven Release workflow should start.`);

  if (!options.noWatch) {
    const runId = await waitForReleaseRun(plan.sha, previousRun);
    run("gh", ["run", "watch", String(runId), "--repo", REPOSITORY, "--exit-status"]);
    verifyPublishedRelease(plan.tag, plan.sha, plan.kind === "rc");
    console.log(`Released ${plan.tag}`);
  }
}

function parseArgs(args: string[]): Options {
  if (args.length === 0 || args.includes("--help") || args.includes("-h")) {
    console.log(usage());
    process.exit(0);
  }
  const command = args[0];
  if (command !== "status" && command !== "rc" && command !== "stable") {
    fail(`Unknown command: ${command}\n\n${usage()}`);
  }
  const options: Options = { command, dryRun: false, noPush: false, noWatch: false, from: null };
  for (let i = 1; i < args.length; i++) {
    const arg = args[i]!;
    if (arg === "--dry-run") options.dryRun = true;
    else if (arg === "--no-push") options.noPush = true;
    else if (arg === "--no-watch") options.noWatch = true;
    else if (arg === "--from") {
      const value = args[++i];
      if (!value) fail("--from requires an RC tag");
      options.from = value;
    } else fail(`Unknown option: ${arg}\n\n${usage()}`);
  }
  if (command !== "stable" && options.from !== null) fail("--from is supported only by stable");
  return options;
}

function usage(): string {
  return `Usage:
  bun scripts/release.ts status
  bun scripts/release.ts rc [--dry-run] [--no-push] [--no-watch]
  bun scripts/release.ts stable [--from fork-YYYYMMDD.N-rcN] [--dry-run] [--no-push] [--no-watch]

rc:
  Continue an unfinished release line with the next rcN. If the latest line is
  already stable, start the next .N release at rc1.

stable:
  Promote the latest RC (or --from RC) by creating the same-source tag without
  -rcN. The RC remains immutable.

Pushing the exact tag triggers .github/workflows/release.yml. The script refuses
dirty/out-of-date branches, wrong fork identity, missing exact-source CI, tag
collisions, mutable/unfinished RCs, and sources predating the tag-driven flow.`;
}

function preflight(): Context {
  run("git", ["rev-parse", "--is-inside-work-tree"], { quiet: true });
  if (run("git", ["status", "--porcelain=v1", "--untracked-files=all"], { quiet: true })) {
    fail("Working tree is not clean. Commit or stash changes before release.");
  }
  const branch = run("git", ["branch", "--show-current"], { quiet: true });
  const match = branch.match(BRANCH);
  if (!match) fail(`Release must run from fork/main.YYYYMMDD. Current: ${branch || "detached"}`);

  const meta = ghJson(["api", `repos/${REPOSITORY}`]) as {
    id?: number; fork?: boolean; parent?: { full_name?: string }; default_branch?: string;
  };
  if (meta.id !== REPOSITORY_ID || meta.fork !== true || meta.parent?.full_name !== UPSTREAM) {
    fail("Repository identity is not the maintained native GitHub fork.");
  }
  if (meta.default_branch !== branch) fail(`Release must run on default generation ${meta.default_branch}. Current: ${branch}`);

  const origin = run("git", ["remote", "get-url", "origin"], { quiet: true });
  if (!origin.includes("yoyooyooo/agent-git-service")) fail(`origin is not ${REPOSITORY}: ${origin}`);
  const pushUrl = run("git", ["remote", "get-url", "--push", "origin"], { quiet: true });
  if (pushUrl === "DISABLED") fail("origin push is disabled in this checkout.");

  run("git", ["fetch", "--no-tags", "origin", branch], { quiet: true });
  const head = run("git", ["rev-parse", "HEAD"], { quiet: true });
  const remoteHead = run("git", ["rev-parse", `refs/remotes/origin/${branch}`], { quiet: true });
  if (head !== remoteHead) fail(`Local ${branch} is not exactly origin/${branch}.`);
  return { branch, date: match[1]!, head };
}

export function parseReleaseTag(value: string): ParsedTag | null {
  const match = value.match(TAG);
  if (!match) return null;
  return { raw: value, date: match[1]!, serial: Number(match[2]), rc: match[3] ? Number(match[3]) : null };
}

function readReleaseTags(): ParsedTag[] {
  const output = run("git", ["ls-remote", "--tags", "origin", "refs/tags/fork-*"], { quiet: true });
  return output.split(/\r?\n/).filter(Boolean)
    .map((line) => line.split(/\s+/)[1] ?? "")
    .filter((ref) => ref.startsWith("refs/tags/") && !ref.endsWith("^{}"))
    .map((ref) => parseReleaseTag(ref.slice("refs/tags/".length)))
    .filter((tag): tag is ParsedTag => tag !== null);
}

export function releaseLines(tags: ParsedTag[], date: string): ReleaseLine[] {
  const grouped = new Map<number, ReleaseLine>();
  for (const tag of tags.filter((row) => row.date === date)) {
    let line = grouped.get(tag.serial);
    if (!line) {
      line = { serial: tag.serial, stable: null, rcs: [] };
      grouped.set(tag.serial, line);
    }
    if (tag.rc === null) line.stable = tag.raw;
    else line.rcs.push(tag);
  }
  for (const line of grouped.values()) {
    line.rcs.sort((a, b) => (a.rc ?? 0) - (b.rc ?? 0));
    if (line.stable !== null && line.rcs.length === 0) fail(`Stable ${line.stable} has no RC lineage.`);
  }
  return [...grouped.values()].sort((a, b) => a.serial - b.serial);
}

export function chooseNextRc(lines: ReleaseLine[], date: string): string {
  const latest = lines.at(-1);
  if (!latest) return `fork-${date}.1-rc1`;
  if (latest.stable !== null) return `fork-${date}.${latest.serial + 1}-rc1`;
  return `fork-${date}.${latest.serial}-rc${(latest.rcs.at(-1)?.rc ?? 0) + 1}`;
}

export function choosePromotableRc(lines: ReleaseLine[]): string | null {
  const latest = lines.at(-1);
  if (!latest || latest.stable !== null) return null;
  return latest.rcs.at(-1)?.raw ?? null;
}

function planRc(context: Context, lines: ReleaseLine[]): ReleasePlan {
  const tag = chooseNextRc(lines, context.date);
  const latestRc = choosePromotableRc(lines);
  if (latestRc !== null && remoteTagSha(latestRc) === context.head) {
    fail(`HEAD is already published as ${latestRc}. Use stable or change source before another RC.`);
  }
  return { kind: "rc", tag, sha: context.head, fromRc: null };
}

function planStable(context: Context, lines: ReleaseLine[], from: string | null): ReleasePlan {
  const rcTag = from ?? choosePromotableRc(lines);
  if (rcTag === null) fail("No unpromoted RC exists for the current generation.");
  const parsed = parseReleaseTag(rcTag);
  if (!parsed || parsed.rc === null || parsed.date !== context.date) fail(`Not an RC from generation ${context.date}: ${rcTag}`);
  const stable = `fork-${parsed.date}.${parsed.serial}`;
  if (lines.find((line) => line.serial === parsed.serial)?.stable) fail(`Stable ${stable} already exists.`);
  const sha = remoteTagSha(rcTag);
  if (!sha) fail(`Remote RC tag does not exist: ${rcTag}`);
  return { kind: "stable", tag: stable, sha, fromRc: rcTag };
}

function formatPlan(plan: ReleasePlan, options: Options): string {
  return `Release plan:
  kind: ${plan.kind}
  tag: ${plan.tag}
  source: ${plan.sha}
  from RC: ${plan.fromRc ?? "n/a"}
  pushes: ${options.dryRun ? "none (dry-run)" : options.noPush ? "local tag only" : "exact remote tag only"}
  watch: ${options.noWatch ? "no" : "yes"}`;
}

function verifyExactCI(sha: string): void {
  const response = ghJson(["api", `repos/${REPOSITORY}/actions/runs?head_sha=${sha}&per_page=100`]) as {
    workflow_runs?: Array<{ id: number; head_sha: string; path: string; status: string; conclusion: string; repository: { id: number } }>;
  };
  for (const [path, minJobs] of [[CI_WORKFLOW, 10], [SECRET_WORKFLOW, 1]] as const) {
    const eligible = (response.workflow_runs ?? []).filter((row) => row.head_sha === sha && row.path === path &&
      row.status === "completed" && row.conclusion === "success" && row.repository.id === REPOSITORY_ID);
    if (eligible.length === 0) fail(`Exact source ${sha} lacks successful ${path}.`);
    const chosen = eligible.sort((a, b) => b.id - a.id)[0]!;
    const jobs = ghJson(["api", `repos/${REPOSITORY}/actions/runs/${chosen.id}/jobs?per_page=100`]) as {
      total_count: number; jobs: Array<{ conclusion: string }>;
    };
    if (jobs.total_count !== jobs.jobs.length || jobs.total_count < minJobs || jobs.jobs.some((job) => job.conclusion !== "success")) {
      fail(`Exact source ${sha} has incomplete/skipped jobs in ${path}.`);
    }
  }
}

function ensureTagDrivenWorkflow(sha: string): void {
  const text = run("git", ["show", `${sha}:${RELEASE_WORKFLOW}`], { quiet: true });
  if (!text.includes("tags:") || !text.includes("fork-*") || text.includes("workflow_dispatch:")) {
    fail(`Source ${sha} does not contain the accepted tag-driven Release workflow.`);
  }
}

function ensureTagAvailable(tag: string): void {
  if (remoteTagSha(tag) !== null) fail(`Remote tag already exists: ${tag}`);
  if (ghJsonMaybe(["api", `repos/${REPOSITORY}/releases/tags/${tag}`]) !== null) fail(`GitHub Release already exists: ${tag}`);
}

function verifyPromotableRc(tag: string, sha: string): void {
  const release = ghJsonMaybe(["api", `repos/${REPOSITORY}/releases/tags/${tag}`]) as null | {
    draft?: boolean; prerelease?: boolean; immutable?: boolean; target_commitish?: string;
  };
  if (!release || release.draft || !release.prerelease || !release.immutable || release.target_commitish !== sha) {
    fail(`${tag} is not a published immutable prerelease for exact source ${sha}.`);
  }
}

function remoteTagSha(tag: string): string | null {
  const output = run("git", ["ls-remote", "origin", `refs/tags/${tag}`, `refs/tags/${tag}^{}`], { quiet: true });
  if (!output) return null;
  const rows = output.split(/\r?\n/).filter(Boolean).map((line) => line.split(/\s+/));
  return (rows.find((row) => row[1]?.endsWith("^{}")) ?? rows[0])?.[0] ?? null;
}

function createLocalTag(tag: string, sha: string): void {
  if (exec("git", ["rev-parse", "-q", "--verify", `refs/tags/${tag}`], false).exitCode === 0) {
    if (run("git", ["rev-list", "-n", "1", tag], { quiet: true }) !== sha) fail(`Local tag ${tag} points elsewhere.`);
    return;
  }
  run("git", ["tag", tag, sha], { quiet: true });
}

function pushExactTag(tag: string, sha: string): void {
  run("git", ["push", "origin", `${sha}:refs/tags/${tag}`]);
  if (remoteTagSha(tag) !== sha) fail(`Remote tag readback mismatch after push: ${tag}`);
}

function latestReleaseRunId(): number {
  const rows = ghJson(["run", "list", "--repo", REPOSITORY, "--workflow", "release.yml", "--limit", "20",
    "--json", "databaseId"]) as Array<{ databaseId: number }>;
  return rows.reduce((max, row) => Math.max(max, row.databaseId), 0);
}

async function waitForReleaseRun(sha: string, previousRun: number): Promise<number> {
  const deadline = Date.now() + 90_000;
  while (Date.now() < deadline) {
    const rows = ghJson(["run", "list", "--repo", REPOSITORY, "--workflow", "release.yml", "--limit", "20",
      "--json", "databaseId,headSha,event,status,conclusion"]) as Array<{
        databaseId: number; headSha: string; event: string; status: string; conclusion: string;
      }>;
    const match = rows.filter((row) => row.databaseId > previousRun && row.headSha === sha && row.event === "push")
      .sort((a, b) => b.databaseId - a.databaseId)[0];
    if (match) return match.databaseId;
    await Bun.sleep(3000);
  }
  fail("Tag was pushed but no matching Release workflow appeared within 90 seconds.");
}

function verifyPublishedRelease(tag: string, sha: string, prerelease: boolean): void {
  const release = ghJson(["api", `repos/${REPOSITORY}/releases/tags/${tag}`]) as {
    draft?: boolean; prerelease?: boolean; immutable?: boolean; target_commitish?: string; assets?: Array<{ name?: string }>;
  };
  if (release.draft || release.prerelease !== prerelease || !release.immutable || release.target_commitish !== sha) {
    fail(`Published Release identity/state mismatch for ${tag}.`);
  }
  const assets = new Set((release.assets ?? []).map((asset) => asset.name));
  for (const required of ["install.sh", "SHA256SUMS"]) if (!assets.has(required)) fail(`${tag} missing ${required}.`);
}

function ghJson(args: string[]): unknown {
  const result = exec("gh", args, false);
  if (result.exitCode !== 0) fail(`GitHub command failed: gh ${args.join(" ")}\n${result.stderr || result.stdout}`);
  return JSON.parse(result.stdout || "null") as unknown;
}

function ghJsonMaybe(args: string[]): unknown | null {
  const result = exec("gh", args, false);
  if (result.exitCode === 0) return JSON.parse(result.stdout || "null") as unknown;
  if (result.stderr.includes("HTTP 404") || result.stderr.includes("Not Found")) return null;
  fail(`GitHub command failed: gh ${args.join(" ")}\n${result.stderr || result.stdout}`);
}

function run(command: string, args: string[], options: { quiet?: boolean } = {}): string {
  const result = exec(command, args, options.quiet !== true);
  if (result.exitCode !== 0) fail(`Command failed: ${[command, ...args].join(" ")}\n${result.stderr || result.stdout}`);
  return result.stdout.trim();
}

function exec(command: string, args: string[], log: boolean): { exitCode: number; stdout: string; stderr: string } {
  if (log) console.log(`$ ${[command, ...args].join(" ")}`);
  const result = spawnSync(command, args, { encoding: "utf8" });
  return { exitCode: result.status ?? 1, stdout: result.stdout ?? "", stderr: result.stderr ?? "" };
}

function fail(message: string): never { throw new Error(message); }
