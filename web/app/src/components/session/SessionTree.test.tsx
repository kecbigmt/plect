import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { SessionTree } from "@/components/session/SessionTree";
import type { SessionSummary } from "@/lib/sessionsApi";

function summary(overrides: Partial<SessionSummary> & { sessionName: string }): SessionSummary {
  return {
    displayStatus: "up",
    run: "up",
    resourceId: "",
    ...overrides,
  };
}

const sessions: SessionSummary[] = [
  summary({ sessionName: "release" }),
  summary({ sessionName: "release/config", parentSession: "release" }),
  summary({ sessionName: "release/config/compat", parentSession: "release/config" }),
  summary({ sessionName: "docs" }),
];

function renderTree(props: Partial<React.ComponentProps<typeof SessionTree>> = {}) {
  const onToggleExpanded = vi.fn();
  const onSelect = vi.fn();
  const utils = render(
    <SessionTree
      sessions={sessions}
      selectedName={null}
      expandedNames={new Set()}
      query=""
      onToggleExpanded={onToggleExpanded}
      onSelect={onSelect}
      {...props}
    />,
  );
  return { onToggleExpanded, onSelect, ...utils };
}

describe("SessionTree", () => {
  it("renders top-level rows collapsed by default, hiding unexpanded descendants", () => {
    renderTree();
    expect(screen.getByRole("treeitem", { name: /^release$/ })).toBeInTheDocument();
    expect(screen.getByRole("treeitem", { name: /^docs$/ })).toBeInTheDocument();
    expect(screen.queryByRole("treeitem", { name: /release\/config$/ })).not.toBeInTheDocument();
  });

  it("expanding a row is independent from selecting it", async () => {
    const user = userEvent.setup();
    const { onSelect, onToggleExpanded } = renderTree();
    await user.click(screen.getByRole("button", { name: /expand release/i }));
    expect(onToggleExpanded).toHaveBeenCalledWith("release");
    expect(onSelect).not.toHaveBeenCalled();
  });

  it("selecting a row does not implicitly expand it", async () => {
    const user = userEvent.setup();
    const { onSelect, onToggleExpanded } = renderTree();
    await user.click(screen.getByRole("treeitem", { name: /^release$/ }));
    expect(onSelect).toHaveBeenCalledWith("release");
    expect(onToggleExpanded).not.toHaveBeenCalled();
  });

  it("marks the selected row aria-selected, independent of expansion state", () => {
    renderTree({ selectedName: "docs" });
    expect(screen.getByRole("treeitem", { name: /^docs$/ })).toHaveAttribute("aria-selected", "true");
    expect(screen.getByRole("treeitem", { name: /^release$/ })).toHaveAttribute("aria-selected", "false");
  });

  it("reveals a deeper row once its ancestors are in expandedNames", () => {
    renderTree({ expandedNames: new Set(["release", "release/config"]) });
    expect(screen.getByRole("treeitem", { name: /release\/config\/compat$/ })).toBeInTheDocument();
  });

  it("disables the expand control on a leaf row", () => {
    renderTree();
    expect(screen.getByRole("button", { name: /expand docs/i })).toBeDisabled();
  });

  it("with a search query, shows only matches and their ancestors, force-expanded", () => {
    renderTree({ query: "compat" });
    expect(screen.getByRole("treeitem", { name: /^release$/ })).toBeInTheDocument();
    expect(screen.getByRole("treeitem", { name: /release\/config$/ })).toBeInTheDocument();
    expect(screen.getByRole("treeitem", { name: /compat$/ })).toBeInTheDocument();
    expect(screen.queryByRole("treeitem", { name: /^docs$/ })).not.toBeInTheDocument();
  });

  it("shows an empty state when the query matches nothing", () => {
    renderTree({ query: "no-such-session" });
    expect(screen.queryAllByRole("treeitem")).toHaveLength(0);
    expect(screen.getByText(/no sessions match/i)).toBeInTheDocument();
  });

  it("shows an empty state when there are no sessions at all", () => {
    renderTree({ sessions: [] });
    expect(screen.getByText(/no sessions/i)).toBeInTheDocument();
  });

  describe("keyboard navigation", () => {
    it("ArrowDown moves roving focus to the next visible row", async () => {
      const user = userEvent.setup();
      renderTree({ expandedNames: new Set(["release"]) });
      const release = screen.getByRole("treeitem", { name: /^release$/ });
      release.focus();
      await user.keyboard("{ArrowDown}");
      expect(screen.getByRole("treeitem", { name: /release\/config$/ })).toHaveFocus();
    });

    it("ArrowUp moves roving focus to the previous visible row", async () => {
      const user = userEvent.setup();
      renderTree({ expandedNames: new Set(["release"]) });
      const config = screen.getByRole("treeitem", { name: /release\/config$/ });
      config.focus();
      await user.keyboard("{ArrowUp}");
      expect(screen.getByRole("treeitem", { name: /^release$/ })).toHaveFocus();
    });

    it("ArrowRight expands a collapsed row with children instead of moving focus", async () => {
      const user = userEvent.setup();
      const { onToggleExpanded } = renderTree();
      screen.getByRole("treeitem", { name: /^release$/ }).focus();
      await user.keyboard("{ArrowRight}");
      expect(onToggleExpanded).toHaveBeenCalledWith("release");
    });

    it("ArrowLeft collapses an expanded row with children instead of moving focus", async () => {
      const user = userEvent.setup();
      const { onToggleExpanded } = renderTree({ expandedNames: new Set(["release"]) });
      screen.getByRole("treeitem", { name: /^release$/ }).focus();
      await user.keyboard("{ArrowLeft}");
      expect(onToggleExpanded).toHaveBeenCalledWith("release");
    });

    it("ArrowLeft on a leaf or collapsed row moves focus to its parent", async () => {
      const user = userEvent.setup();
      renderTree({ expandedNames: new Set(["release"]) });
      screen.getByRole("treeitem", { name: /release\/config$/ }).focus();
      await user.keyboard("{ArrowLeft}");
      expect(screen.getByRole("treeitem", { name: /^release$/ })).toHaveFocus();
    });

    it("Enter selects the focused row", async () => {
      const user = userEvent.setup();
      const { onSelect } = renderTree();
      screen.getByRole("treeitem", { name: /^docs$/ }).focus();
      await user.keyboard("{Enter}");
      expect(onSelect).toHaveBeenCalledWith("docs");
    });

    it("only the roving row is in the tab sequence", () => {
      renderTree({ selectedName: "docs" });
      expect(screen.getByRole("treeitem", { name: /^docs$/ })).toHaveAttribute("tabIndex", "0");
      expect(screen.getByRole("treeitem", { name: /^release$/ })).toHaveAttribute("tabIndex", "-1");
    });
  });
});
