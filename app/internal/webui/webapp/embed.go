// Package webapp embeds the production build of the React/TypeScript Web UI
// shell (web/app, built by Vite). The build is committed rather than
// produced at install time — go:embed can only see files present at compile
// time, and the go install github.com/kecbigmt/plecture/app/cmd/plect@latest
// installability invariant compiles from committed module source alone, the
// same reason web/api's generated OpenAPI/TypeScript/Go contract types are
// committed instead of regenerated on install. Regenerate it after changing
// web/app's source:
//
//	cd web/app && pnpm install --frozen-lockfile && pnpm build
package webapp

import "embed"

//go:embed dist
var FS embed.FS
