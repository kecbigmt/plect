//go:build !cgo

package persistence

// github.com/mattn/go-sqlite3 compiles without cgo by selecting a stub
// implementation that builds successfully but fails at runtime the moment a
// database is opened — silently shipping that stub in a released core
// binary would turn every SQLite open into a late runtime surprise instead
// of a build failure a maintainer sees immediately. Referencing this
// undefined identifier turns the failure into a compile error, and the
// identifier's own name is the actionable message: set CGO_ENABLED=1 and
// build with a C toolchain, or build from a machine that has one.
var _ = plect_core_requires_CGO_ENABLED_1_set_CGO_ENABLED_1_or_build_with_a_C_toolchain
