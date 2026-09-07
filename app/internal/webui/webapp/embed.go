// Package webapp exposes FS, the filesystem served under /app/ by
// app/internal/webui's server. dist/ (web/app's Vite output) is
// gitignored, and go:embed of a directory with zero matching files is a
// compile error, not a runtime one — so FS has two build-tagged sources:
// embed_dist.go (-tags webembed) embeds a real, pre-built dist/; the
// default, embed_placeholder.go, embeds a tiny committed stand-in so
// every other build/test of this module stays Node-free. Both root FS at
// the shell's own contents, so callers need not know which one is active.
package webapp

import "io/fs"

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err) // the embedded build always contains this directory; a missing one is a build bug, not a runtime condition.
	}
	return sub
}
