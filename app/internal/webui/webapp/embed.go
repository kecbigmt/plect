// Package webapp embeds the Web UI shell served under /app/. static/dist/
// (Vite's output) is gitignored and entirely untracked; static/unbuilt.html,
// its tracked sibling, keeps go:embed — a compile-time check, not a
// runtime one — always satisfied. app/internal/webui/server.go checks at
// runtime whether static/dist/index.html — a real build — is present.
package webapp

import "embed"

//go:embed static
var FS embed.FS
