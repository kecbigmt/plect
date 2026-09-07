package commands

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/datahome"
)

// TestDataHomeFlag_OverridesDefaultDataDirectory exercises --data-home the
// way TestConfigShow_FlagWinsOverEnvVar exercises --config-home: `storage
// migrate` reports persistence.DefaultPath(), which must resolve under the
// flag's directory once PersistentPreRunE has exported it to
// $PLECT_DATA_HOME.
func TestDataHomeFlag_OverridesDefaultDataDirectory(t *testing.T) {
	t.Setenv(datahome.EnvVar, "")
	t.Setenv(datahome.XDGEnvVar, "")
	flagDir := t.TempDir()
	t.Cleanup(func() { dataHomeFlag = "" })

	out, err := execRoot(t, "--data-home", flagDir, "storage", "migrate")
	if err != nil {
		t.Fatalf("Execute() error = %v; output:\n%s", err, out)
	}
	want := filepath.Join(flagDir, "storage.db")
	if !strings.Contains(out, want) {
		t.Errorf("output = %q, want to contain %q", out, want)
	}
}
