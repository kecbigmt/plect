package webui

import (
	"os"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/confighome"
)

// TestMain unsets XDG_CONFIG_HOME before any test runs: a runner environment
// that predefines it (GitHub Actions' ubuntu runners do) would otherwise leak
// through tests that fake HOME via t.Setenv but never touch XDG_CONFIG_HOME.
// Tests that want to simulate it opt back in with t.Setenv.
func TestMain(m *testing.M) {
	os.Unsetenv(confighome.XDGEnvVar)
	os.Exit(runTestMain(m))
}
