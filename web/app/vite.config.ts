import { fileURLToPath } from "node:url";
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// The Go server mounts the built assets at /app/ (see
// app/internal/webui/server.go); base must match so emitted asset URLs
// resolve there instead of at the origin root, which /  still serves (the
// existing htmx UI).
//
// server.proxy forwards everything the shell needs from the dev server to a
// separately running `plect-web` so both sit on one origin during
// development too: the plect_csrf/plect_auth cookies are HttpOnly and
// SameSite=Strict, so a cross-origin dev server could neither receive nor
// resend them. PLECT_WEB_ORIGIN overrides the default target.
const backendOrigin = process.env.PLECT_WEB_ORIGIN ?? "http://127.0.0.1:8787";

export default defineConfig({
  base: "/app/",
  plugins: [react(), tailwindcss()],
  resolve: { alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) } },
  build: {
    // Gitignored; go:embed'd from here by a -tags webembed build — see
    // app/internal/webui/webapp.
    outDir: "../../app/internal/webui/webapp/dist",
    emptyOutDir: true,
  },
  server: {
    proxy: {
      "/api": backendOrigin,
      "/login": backendOrigin,
      "/healthz": backendOrigin,
    },
  },
});
