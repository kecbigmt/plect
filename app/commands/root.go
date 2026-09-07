package commands

import (
	"fmt"
	"os"

	"github.com/kecbigmt/plecture/app/internal/confighome"
	"github.com/kecbigmt/plecture/app/internal/persistence"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/spf13/cobra"
)

var configHomeFlag string

var rootCmd = &cobra.Command{
	Use:   "plect",
	Short: "Manage runtime sessions and workdirs for resource-driven workflows",
	Long: `plect manages runtime sessions and workdirs for workflows keyed by a
resource identifier.

A workflow's [resolver] maps the resource identifier to a session id and
working directory; plect then runs the workflow's tasks (workdir setup, runtime
session, agent launch) and manages the full session lifecycle.

The resource identifier is any string a workflow resolver accepts. Which
identifiers resolve out of the box depends on the providers installed; an
identifier no resolver matches selects a workflow explicitly (see
'plect workflow list').`,
	// Cobra prints usage after any RunE error by default, which is noise for
	// runtime failures (a failed task is not an argument-parsing mistake).
	// We still want the "Error: ..." line that Cobra prints, just not the
	// help dump that follows.
	SilenceUsage:  true,
	SilenceErrors: true,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		// Exporting to the env var, rather than threading the flag value through
		// every config/plugins path resolver, keeps confighome.Resolve a plain
		// env lookup at every call site.
		if configHomeFlag != "" {
			if err := os.Setenv(confighome.EnvVar, configHomeFlag); err != nil {
				return fmt.Errorf("set %s from --config-home: %w", confighome.EnvVar, err)
			}
		}
		if cmd == storageMigrateCmd {
			// storageMigrateCmd's own RunE performs this exact currency
			// check, choosing EnsureCurrent or EnsureCurrentAllowDevBuild by
			// its --allow-dev-build flag; running the strict variant here
			// first would refuse before that flag ever took effect.
			return nil
		}
		if cmd == storageImportCmd {
			// legacyimport.Run targets a directory with no live database yet
			// (often the same default path this hook would otherwise touch)
			// and refuses if one already exists; auto-creating an empty
			// storage.db here first would make that refusal fire on every
			// invocation, including the documented default cutover command.
			return nil
		}
		if cmd == storageRepairCmd {
			// legacyimport.RepairImportedSessions must back up --data-home's
			// storage.db before anything opens or migrates it; opening the
			// default path here first (--data-home or not) would risk a
			// silent migration landing ahead of that backup.
			return nil
		}
		if err := state.NewStore("").CheckReadable(); err != nil {
			return err
		}
		// Keeps storage.db's own schema current independently of the
		// state.json check above, which covers session/task state only.
		db, err := persistence.EnsureCurrent(cmd.Context(), persistence.DefaultPath())
		if err != nil {
			return err
		}
		return db.Close()
	},
}

func init() {
	rootCmd.PersistentFlags().StringVar(&configHomeFlag, "config-home", "",
		"Override the config home directory (declarations only: config.toml, catalogs.toml, "+
			"plect.lock, tasks/, workflows/, ...); else $"+confighome.EnvVar+
			", else $"+confighome.XDGEnvVar+"/plect, else ~/.config/plect. "+
			"Runtime state and the plugin cache always resolve from the XDG data/cache dirs, unaffected by this.")
}

func Execute() error {
	err := rootCmd.Execute()
	if err == nil {
		return nil
	}
	if hint := removedLifecycleCommandHint(os.Args[1:]); hint != "" {
		err = fmt.Errorf("%w\n%s", err, hint)
	}
	rootCmd.PrintErrln(rootCmd.ErrPrefix(), err.Error())
	return err
}

func removedLifecycleCommandHint(args []string) string {
	if len(args) == 0 {
		return ""
	}
	switch args[0] {
	case "create":
		return "Use `plect up <resource-id>` instead."
	default:
		return ""
	}
}
