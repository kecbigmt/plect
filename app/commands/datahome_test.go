package commands

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/datahome"
)

// `storage migrate` reports persistence.DefaultPath() directly, unlike `ls`
// which would need a config home too.
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
