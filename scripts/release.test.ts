import { describe, expect, test } from "bun:test";
import {
  chooseNextRc,
  choosePromotableRc,
  parseReleaseTag,
  releaseLines,
} from "./release";

describe("downstream release version decisions", () => {
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
    expect(chooseNextRc(lines, "20260924")).toBe("fork-20260924.2-rc1");
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
