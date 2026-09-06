package mcpserver

import (
	"os"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/confighome"
)

// TestMain unsets PLECT_CONFIG_HOME and XDG_CONFIG_HOME before any test
// runs: either one, forced at the process level (a runner environment, or a
// developer's own shell), would otherwise leak through tests that fake HOME
// via t.Setenv but never touch these — both outrank HOME in
// confighome.Resolve()'s precedence. Tests that want to simulate either opt
// back in with t.Setenv.
func TestMain(m *testing.M) {
	os.Unsetenv(confighome.EnvVar)
	os.Unsetenv(confighome.XDGEnvVar)
	os.Exit(m.Run())
}
