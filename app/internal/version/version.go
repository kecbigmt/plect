// Package version holds the running plect binary's version string.
package version

// Current is the running plect version, compared against a plugin's
// declared plect_min_version. It is a hardcoded placeholder because plect
// has no release/tagging process yet; wire this to build-time injection
// (ldflags) once one exists, without changing any caller of Current.
const Current = "0.0.0-dev"

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
