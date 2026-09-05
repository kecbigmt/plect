import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { AppShell } from "@/components/shell/AppShell";

const bootstrap = { apiVersion: "v1", csrfToken: "tok-1" };

function stubMatchMedia(matches: boolean) {
  vi.stubGlobal(
    "matchMedia",
    vi.fn().mockReturnValue({
      matches,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    }),
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("AppShell wide layout", () => {
  beforeEach(() => stubMatchMedia(true));

  it("keeps the sidebar as a persistent column, not an overlay trigger", () => {
    render(<AppShell bootstrap={bootstrap} />);
    expect(screen.getByRole("navigation", { name: /sessions/i })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /open sessions/i })).not.toBeInTheDocument();
  });

  it("opens the detail pane as a persistent column on toggle", async () => {
    const user = userEvent.setup();
    render(<AppShell bootstrap={bootstrap} />);
    expect(screen.queryByRole("complementary", { name: /details/i })).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /toggle details/i }));
    expect(screen.getByRole("complementary", { name: /details/i })).toBeInTheDocument();
  });
});

describe("AppShell narrow layout", () => {
  beforeEach(() => stubMatchMedia(false));

  it("hides the sidebar behind an overlay trigger instead of a persistent column", () => {
    render(<AppShell bootstrap={bootstrap} />);
    expect(screen.queryByRole("navigation", { name: /sessions/i })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /open sessions/i })).toBeInTheDocument();
  });

  it("opens the sidebar overlay from its trigger", async () => {
    const user = userEvent.setup();
    render(<AppShell bootstrap={bootstrap} />);
    await user.click(screen.getByRole("button", { name: /open sessions/i }));
    expect(await screen.findByRole("navigation", { name: /sessions/i })).toBeInTheDocument();
  });
});
