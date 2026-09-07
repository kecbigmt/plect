// Package version holds the running plect binary's version string.
package version

// Current is the running plect version, compared against a plugin's
// declared plect_min_version. The release workflow overwrites it with the
// released tag via -ldflags -X; every other build keeps the "0.0.0-dev"
// placeholder below. It is a variable, not a constant, only because -X can
// overwrite nothing else.
var Current = "0.0.0-dev"
