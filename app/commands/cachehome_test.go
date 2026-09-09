package commands

import (
	"testing"

	"github.com/kecbigmt/plecture/app/internal/cachehome"
	"github.com/kecbigmt/plecture/app/internal/confighome"
	"github.com/kecbigmt/plecture/app/internal/plugins"
)

// `catalog list` with no registrations succeeds and needs no confirmation,
// so it's a safe no-op command to drive PersistentPreRunE (which exports
// --cache-home into PLECT_CACHE_HOME) through.
func TestCacheHomeFlag_OverridesDefaultCacheRoot(t *testing.T) {
	// PersistentPreRunE opens persistence.DefaultPath() (under datahome's
	// resolution) for every command below; a fresh HOME keeps that off the
	// real, already-migrated production database.
	t.Setenv("HOME", t.TempDir())
	t.Setenv(cachehome.EnvVar, "")
	t.Setenv(cachehome.XDGEnvVar, "")
	// Isolate from the real config home too, so this test cannot read (or be
	// affected by) a real catalogs.toml on the machine it runs on.
	t.Setenv(confighome.EnvVar, t.TempDir())
	flagDir := t.TempDir()
	t.Cleanup(func() { cacheHomeFlag = "" })

	if out, err := execRoot(t, "--cache-home", flagDir, "catalog", "list"); err != nil {
		t.Fatalf("Execute() error = %v; output:\n%s", err, out)
	}
	if got := plugins.DefaultCacheRoot(); got != flagDir {
		t.Errorf("DefaultCacheRoot() = %q, want %q", got, flagDir)
	}
}
