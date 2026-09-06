import { describe, expect, it } from "vitest";

import { isWebUrl } from "@/lib/url";

describe("isWebUrl", () => {
  it.each([
    ["https://github.com/owner/repo/issues/7", true],
    ["http://example.com", true],
    ["my-experiment", false],
    ["owner:foo", false],
    ["", false],
    ["ftp://example.com/x", false],
    ["javascript:alert(1)", false],
  ])("isWebUrl(%j) is %s", (input, want) => {
    expect(isWebUrl(input)).toBe(want);
  });
});
