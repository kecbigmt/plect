// Package webapp embeds the Web UI shell served under /app/. dist/ (Vite's
// output) is gitignored except for a placeholder dist/.gitkeep: go:embed
// fails to compile on a directory with zero matching files, so something
// must always be there. app/internal/webui/server.go checks at runtime
// whether dist/index.html — a real build — is actually present.
package webapp

import "embed"

//go:embed all:dist
var FS embed.FS
