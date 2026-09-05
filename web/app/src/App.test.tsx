import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import { App } from "@/App";

function renderApp() {
  const queryClient = new QueryClient();
  return render(
    <QueryClientProvider client={queryClient}>
      <App />
    </QueryClientProvider>,
  );
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

beforeEach(() => {
  vi.stubGlobal("fetch", vi.fn());
  // AppShell reads matchMedia to pick the wide/narrow layout; jsdom has no
  // implementation, so default to "wide" unless a test overrides it.
  vi.stubGlobal(
    "matchMedia",
    vi.fn().mockReturnValue({
      matches: true,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    }),
  );
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("App", () => {
  it("shows a loading state before bootstrap resolves", () => {
    vi.mocked(fetch).mockReturnValue(new Promise(() => {})); // never resolves
    renderApp();
    expect(screen.getByRole("status")).toHaveTextContent(/connecting/i);
  });

  it("renders the workspace shell once bootstrap succeeds", async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse({ apiVersion: "v1", csrfToken: "tok-1" }));
    renderApp();
    await waitFor(() => expect(screen.getByText("Plecture")).toBeInTheDocument());
    expect(screen.getByText("API v1")).toBeInTheDocument();
    // Closed initially, per docs/design/web-ui.md's shared-details contract.
    expect(screen.getByRole("button", { name: /toggle details/i })).toHaveAttribute(
      "aria-expanded",
      "false",
    );
  });

  it("links to sign-in when bootstrap reports unauthenticated", async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse({ message: "authentication required" }, 401));
    renderApp();
    const link = await screen.findByRole("link", { name: /go to sign in/i });
    expect(link).toHaveAttribute("href", expect.stringMatching(/^\/login\?next=/));
  });

  it("offers a retry when the server is unavailable, and retry re-fetches", async () => {
    vi.mocked(fetch).mockRejectedValue(new TypeError("network error"));
    renderApp();
    await screen.findByRole("alert");
    expect(screen.getByText(/unavailable/i)).toBeInTheDocument();

    vi.mocked(fetch).mockResolvedValue(jsonResponse({ apiVersion: "v1", csrfToken: "tok-1" }));
    screen.getByRole("button", { name: /retry/i }).click();

    await waitFor(() => expect(screen.getByText("Plecture")).toBeInTheDocument());
  });
});
