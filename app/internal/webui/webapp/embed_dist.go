//go:build webembed

package webapp

import "embed"

//go:embed dist
var rawDist embed.FS

// FS is the production Vite build produced by web/app.
var FS = mustSub(rawDist, "dist")
