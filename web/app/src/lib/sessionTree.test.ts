import { describe, expect, it } from "vitest";

import {
  ancestorNames,
  buildSessionForest,
  matchesSessionQuery,
  visibleTreeRows,
} from "@/lib/sessionTree";
import type { SessionSummary } from "@/lib/sessionsApi";

function summary(overrides: Partial<SessionSummary> & { sessionName: string }): SessionSummary {
  return {
    displayStatus: "up",
    run: "up",
    resourceId: "",
    ...overrides,
  };
}

describe("buildSessionForest", () => {
  it("nests a session under its real parentSession", () => {
    const sessions = [
      summary({ sessionName: "team/a" }),
      summary({ sessionName: "team/a/review", parentSession: "team/a" }),
    ];
    const forest = buildSessionForest(sessions);
    expect(forest.map((n) => n.session.sessionName)).toEqual(["team/a"]);
    expect(forest[0].children.map((n) => n.session.sessionName)).toEqual(["team/a/review"]);
  });

  it("keeps a parentless session as its own independent root, not grouped with other parentless sessions", () => {
    const sessions = [summary({ sessionName: "alpha" }), summary({ sessionName: "beta" })];
    const forest = buildSessionForest(sessions);
    expect(forest.map((n) => n.session.sessionName)).toEqual(["alpha", "beta"]);
    expect(forest.every((n) => n.children.length === 0)).toBe(true);
  });

  it("places an explicit root:<name> sibling at the top level, flat beside the named session rather than nested under it", () => {
    const sessions = [
      summary({ sessionName: "owner/repo-1" }),
      summary({ sessionName: "owner/repo-reviewer", parentSession: "root:owner/repo-1" }),
    ];
    const forest = buildSessionForest(sessions);
    expect(forest.map((n) => n.session.sessionName).sort()).toEqual([
      "owner/repo-1",
      "owner/repo-reviewer",
    ]);
    expect(forest.every((n) => n.children.length === 0)).toBe(true);
  });

  it("surfaces a session whose declared parent is missing from the fetched list as its own top-level row", () => {
    const sessions = [summary({ sessionName: "orphan", parentSession: "does-not-exist" })];
    const forest = buildSessionForest(sessions);
    expect(forest.map((n) => n.session.sessionName)).toEqual(["orphan"]);
  });

  it("does not derive hierarchy from slashes or other name punctuation", () => {
    const sessions = [
      summary({ sessionName: "team/a" }),
      summary({ sessionName: "team/a/deep/child" }),
    ];
    const forest = buildSessionForest(sessions);
    // Both are parentless (no parentSession set), so both stay top-level
    // independent roots despite one name looking like the other's descendant.
    expect(forest.map((n) => n.session.sessionName)).toEqual(["team/a", "team/a/deep/child"]);
  });

  it("orders siblings by name at every level, independent of input order", () => {
    const sessions = [
      summary({ sessionName: "root/b", parentSession: "root" }),
      summary({ sessionName: "root" }),
      summary({ sessionName: "root/a", parentSession: "root" }),
    ];
    const forest = buildSessionForest(sessions);
    expect(forest[0].children.map((n) => n.session.sessionName)).toEqual(["root/a", "root/b"]);
  });
});

describe("ancestorNames", () => {
  it("walks up real parentSession links, root-to-target order, excluding the target itself", () => {
    const sessions = [
      summary({ sessionName: "a" }),
      summary({ sessionName: "a/b", parentSession: "a" }),
      summary({ sessionName: "a/b/c", parentSession: "a/b" }),
    ];
    expect(ancestorNames(sessions, "a/b/c")).toEqual(["a", "a/b"]);
  });

  it("stops at an explicit root:<name> pseudo-parent without including it as an ancestor", () => {
    const sessions = [
      summary({ sessionName: "owner/repo-1" }),
      summary({ sessionName: "owner/repo-reviewer", parentSession: "root:owner/repo-1" }),
    ];
    expect(ancestorNames(sessions, "owner/repo-reviewer")).toEqual([]);
  });

  it("stops at a missing parent without inventing it as an ancestor", () => {
    const sessions = [summary({ sessionName: "orphan", parentSession: "does-not-exist" })];
    expect(ancestorNames(sessions, "orphan")).toEqual([]);
  });

  it("returns an empty list for an unknown session name", () => {
    expect(ancestorNames([], "nope")).toEqual([]);
  });
});

describe("matchesSessionQuery", () => {
  it("matches on session name, case-insensitively", () => {
    expect(matchesSessionQuery(summary({ sessionName: "team/Alpha" }), "alpha")).toBe(true);
  });

  it("matches on resourceId", () => {
    expect(
      matchesSessionQuery(
        summary({ sessionName: "x", resourceId: "https://example.com/issues/7" }),
        "issues/7",
      ),
    ).toBe(true);
  });

  it("does not match unrelated text", () => {
    expect(matchesSessionQuery(summary({ sessionName: "x", resourceId: "y" }), "z")).toBe(false);
  });

  it("treats an empty query as matching everything", () => {
    expect(matchesSessionQuery(summary({ sessionName: "x" }), "")).toBe(true);
  });
});

describe("visibleTreeRows", () => {
  const sessions = [
    summary({ sessionName: "release" }),
    summary({ sessionName: "release/config", parentSession: "release" }),
    summary({ sessionName: "release/config/compat", parentSession: "release/config" }),
    summary({ sessionName: "docs" }),
  ];
  const forest = buildSessionForest(sessions);

  it("without a query, only expands nodes explicitly in the expanded set", () => {
    const rows = visibleTreeRows(forest, new Set(["release"]), "");
    expect(rows.map((r) => r.session.sessionName)).toEqual(["docs", "release", "release/config"]);
    const config = rows.find((r) => r.session.sessionName === "release/config")!;
    expect(config.hasChildren).toBe(true);
    expect(config.isExpanded).toBe(false);
    expect(config.depth).toBe(1);
  });

  it("with a query, shows only matches plus their ancestors, force-expanded", () => {
    const rows = visibleTreeRows(forest, new Set(), "compat");
    expect(rows.map((r) => r.session.sessionName)).toEqual([
      "release",
      "release/config",
      "release/config/compat",
    ]);
    expect(rows.every((r) => r.isExpanded || !r.hasChildren)).toBe(true);
  });

  it("with a query matching nothing, shows no rows", () => {
    expect(visibleTreeRows(forest, new Set(), "nonexistent")).toEqual([]);
  });
});
