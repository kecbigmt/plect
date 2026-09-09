package commands

import (
	"testing"

	"github.com/kecbigmt/plecture/app/internal/cachehome"
	"github.com/kecbigmt/plecture/app/internal/confighome"
	"github.com/kecbigmt/plecture/app/internal/plugins"
)

// `catalog list` with no registrations needs no confirmation, so it's a safe no-op command to drive PersistentPreRunE (which exports --cache-home into PLECT_CACHE_HOME) through.
func TestCacheHomeFlag_OverridesDefaultCacheRoot(t *testing.T) {
	// A fresh HOME keeps PersistentPreRunE's persistence.DefaultPath() check off the real, already-migrated production database.
	t.Setenv("HOME", t.TempDir())
	t.Setenv(cachehome.EnvVar, "")
	t.Setenv(cachehome.XDGEnvVar, "")
	t.Setenv(confighome.EnvVar, t.TempDir())
	flagDir := t.TempDir()
	t.Cleanup(func() { cacheHomeFlag = "" })

	if out, err := execRoot(t, "--cache-home", flagDir, "catalog", "list"); err != nil {
		t.Fatalf("Execute() error = %v; output:\n%s", err, out)
	}
	got, err := plugins.DefaultCacheRoot()
	if err != nil {
		t.Fatalf("DefaultCacheRoot() error = %v", err)
	}
	if got != flagDir {
		t.Errorf("DefaultCacheRoot() = %q, want %q", got, flagDir)
	}
}
