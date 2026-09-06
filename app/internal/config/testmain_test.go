package config

import (
	"os"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/confighome"
)

// PLECT_CONFIG_HOME and XDG_CONFIG_HOME both outrank HOME in
// confighome.Resolve()'s precedence, so left ambient either would bypass
// every test's HOME-based isolation below.
func TestMain(m *testing.M) {
	os.Unsetenv(confighome.EnvVar)
	os.Unsetenv(confighome.XDGEnvVar)
	os.Exit(m.Run())
}
