//go:build !webembed

package webapp

import "embed"

//go:embed placeholder
var rawPlaceholder embed.FS

// FS is a static stand-in shell, served when this binary was built without
// a prior Vite build (see the package doc for why one is needed at all).
var FS = mustSub(rawPlaceholder, "placeholder")
