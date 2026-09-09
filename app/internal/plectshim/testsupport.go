package plectshim

// UseExecutableForTest swaps the path ForCurrentProcess treats as the
// running executable, and lifts the go-test guard for the duration of a
// test, returning a restore func. Exported (rather than living in a
// _test.go file) so a consumer package's own tests can exercise the real
// shim-building path too.
func UseExecutableForTest(path string) (restore func()) {
	origPath, origForced := executablePath, forcedUnderTest
	executablePath = func() (string, error) { return path, nil }
	forcedUnderTest = true
	return func() { executablePath, forcedUnderTest = origPath, origForced }
}
