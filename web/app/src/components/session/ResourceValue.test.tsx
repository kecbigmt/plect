import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { ResourceValue } from "@/components/session/ResourceValue";

// jsdom defines navigator.clipboard as a real Clipboard instance via a
// getter-only accessor, so vi.stubGlobal("navigator", ...) (a plain
// assignment) silently fails to replace it. Redefining the one property
// directly, and restoring it, is what actually swaps the implementation.
const originalClipboard = navigator.clipboard;

afterEach(() => {
  Object.defineProperty(navigator, "clipboard", { value: originalClipboard, configurable: true });
});

describe("ResourceValue", () => {
  it("renders an http(s) value as a link", () => {
    render(<ResourceValue value="https://example.com/issues/7" />);
    const link = screen.getByRole("link", { name: "https://example.com/issues/7" });
    expect(link).toHaveAttribute("href", "https://example.com/issues/7");
  });

  it("renders a non-web identifier as plain, copyable text rather than a link", () => {
    render(<ResourceValue value="my-experiment" />);
    expect(screen.queryByRole("link")).not.toBeInTheDocument();
    expect(screen.getByText("my-experiment")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /copy/i })).toBeInTheDocument();
  });

  it("copies a non-web identifier to the clipboard on click", async () => {
    // userEvent.setup() installs its own clipboard stub, so it must run
    // before this test replaces navigator.clipboard — the other order lets
    // setup() clobber the mock defined here.
    const user = userEvent.setup();
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    render(<ResourceValue value="acme:foo" />);
    await user.click(screen.getByRole("button", { name: /copy/i }));
    expect(writeText).toHaveBeenCalledWith("acme:foo");
  });

  it("renders an empty value as a placeholder, with no link and no copy button", () => {
    render(<ResourceValue value="" />);
    expect(screen.queryByRole("link")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /copy/i })).not.toBeInTheDocument();
  });
});
