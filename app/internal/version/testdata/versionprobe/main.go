// Command versionprobe prints version.Current. It exists only so
// version_integration_test.go can build it with the exact -ldflags -X
// string the release workflow uses, and so a typo in that import path
// fails a test instead of only surfacing in a published release archive.
package main

import (
	"fmt"

	"github.com/kecbigmt/plecture/app/internal/version"
)

func main() {
	fmt.Print(version.Current)
}
