// Package version holds the running plect binary's version string.
package version

// Current is the running plect version, compared against a plugin's
// declared plect_min_version. A packager building plect from source
// (a release archive's build, or a distribution's own package) overwrites it
// via `-ldflags -X github.com/kecbigmt/plecture/app/internal/version.Current=<version>`;
// every other build keeps the "0.0.0-dev" placeholder below. It is a
// variable, not a constant, only because -X can overwrite nothing else.
var Current = "0.0.0-dev"

// devBuildPlaceholder is Current's value on every build the release
// pipeline did not stamp via -ldflags -X.
const devBuildPlaceholder = "0.0.0-dev"

// IsDevelopmentBuild reports whether this binary carries no release-pipeline
// version stamp: a local `go run`/`go build`, or `go install ...@latest`.
// persistence.EnsureCurrent uses this to decide whether it may forward-migrate
// a database it did not create.
func IsDevelopmentBuild() bool {
	return Current == devBuildPlaceholder
}
