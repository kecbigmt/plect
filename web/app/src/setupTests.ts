import { afterEach } from "vitest";
import { cleanup } from "@testing-library/react";
import "@testing-library/jest-dom/vitest";

// vitest.config.ts does not set test.globals, so @testing-library/react's own
// auto-cleanup (which looks for a global afterEach) never registers; do it
// explicitly instead, so one test's render doesn't leak into the next.
afterEach(() => {
  cleanup();
});
