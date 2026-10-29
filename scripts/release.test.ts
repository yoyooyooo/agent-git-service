import { describe, expect, test } from "bun:test";
import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import {
  chooseNextRc,
  choosePromotableRc,
  latestPublishedTag,
  legacyTagWorkflowRejection,
  parseReleaseTag,
  previousStableTag,
  releaseLines,
} from "./release";

describe("downstream release version decisions", () => {
  test("rc and stable refuse before preflight or any tag mutation", () => {
    for (const command of ["rc", "stable"]) {
      const result = spawnSync(process.execPath, [new URL("release.ts", import.meta.url).pathname, command], {
        cwd: "/", encoding: "utf8",
      });
      expect(result.status).toBe(1);
      expect(result.stderr).toContain("release_source_binding_unverified: tag publication disabled");
      expect(result.stderr).not.toContain("not a git repository");
    }
  });
  test("refuses a tag whose target still has the old release workflow", () => {
    const current = readFileSync(new URL("../.github/workflows/release.yml", import.meta.url), "utf8");
    expect(legacyTagWorkflowRejection(current)).toBeNull();
    const runtime = [
      "name: Release",
      "on:",
      "  push:",
      "    tags:",
      "      - 'fork-*'",
      "jobs:",
      "  build:",
      "    runs-on: ubuntu-24.04",
      "    permissions:",
      "      contents: read",
      "      id-token: write",
      "      attestations: write",
      "    steps:",
      "      - run: python3 fork/scripts/release.py build --version v --output out",
      "  publish:",
      "    permissions:",
      "      contents: write",
      "    steps:",
      "      - run: python3 fork/scripts/release.py publish --version v --sha s --output out",
      "",
    ].join("\n");
    expect(legacyTagWorkflowRejection(runtime)).toContain("release_legacy_tag_refused");
    expect(legacyTagWorkflowRejection(current.replace("exit 65", "exit 0"))).toContain("release_legacy_tag_refused");
    expect(legacyTagWorkflowRejection(current.replace("jobs:", "jobs:\n  build:\n"))).toContain("release_legacy_tag_refused");
  });

  test("parses RC and stable tags only", () => {
    expect(parseReleaseTag("fork-20260924.1-rc6")).toEqual({
      raw: "fork-20260924.1-rc6",
      date: "20260924",
      serial: 1,
      rc: 6,
    });
    expect(parseReleaseTag("fork-20260924.1")).toEqual({
      raw: "fork-20260924.1",
      date: "20260924",
      serial: 1,
      rc: null,
    });
    expect(parseReleaseTag("v1.0.0")).toBeNull();
    expect(parseReleaseTag("fork-20260924.0-rc1")).toBeNull();
  });

  test("starts an empty generation at .1-rc1", () => {
    expect(chooseNextRc([], "20260924")).toBe("fork-20260924.1-rc1");
    expect(choosePromotableRc([])).toBeNull();
  });

  test("continues an unfinished release line", () => {
    const tags = ["fork-20260924.1-rc5", "fork-20260924.1-rc6"]
      .map(parseReleaseTag).filter((tag) => tag !== null);
    const lines = releaseLines(tags, "20260924");
    expect(chooseNextRc(lines, "20260924")).toBe("fork-20260924.1-rc7");
    expect(choosePromotableRc(lines)).toBe("fork-20260924.1-rc6");
  });

  test("a stable release closes the line and starts the next serial", () => {
    const tags = [
      "fork-20260924.1-rc5",
      "fork-20260924.1-rc6",
      "fork-20260924.1",
    ].map(parseReleaseTag).filter((tag) => tag !== null);
    const lines = releaseLines(tags, "20260924");
    expect(choosePromotableRc(lines)).toBeNull();
    expect(latestPublishedTag(lines)).toBe("fork-20260924.1");
    expect(chooseNextRc(lines, "20260924")).toBe("fork-20260924.2-rc1");
  });

  test("tracks the previous stable when a new RC line exists", () => {
    const tags = [
      "fork-20260924.1-rc7",
      "fork-20260924.1",
      "fork-20260924.2-rc1",
    ].map(parseReleaseTag).filter((tag) => tag !== null);
    const lines = releaseLines(tags, "20260924");
    expect(latestPublishedTag(lines)).toBe("fork-20260924.2-rc1");
    expect(previousStableTag(lines, 2)).toBe("fork-20260924.1");
    expect(choosePromotableRc(lines)).toBe("fork-20260924.2-rc1");
  });

  test("other generations do not affect the current release line", () => {
    const tags = [
      "fork-20260923.9",
      "fork-20260924.1-rc1",
      "fork-20261001.1-rc9",
    ].map(parseReleaseTag).filter((tag) => tag !== null);
    const lines = releaseLines(tags, "20260924");
    expect(chooseNextRc(lines, "20260924")).toBe("fork-20260924.1-rc2");
  });
});
