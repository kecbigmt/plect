package commands

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/kecbigmt/plecture/app/internal/legacyimport"
	"github.com/kecbigmt/plecture/app/internal/persistence"
)

var storageCmd = &cobra.Command{
	Use:   "storage",
	Short: "Inspect and maintain plect's local SQLite database",
}

var storageMigrateAllowDevBuild bool

var storageMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Apply any pending schema migrations to the local database",
	Long: `Runs the same migration runner every plect command, plect serve, and
plect-web already run automatically at startup, explicitly. Useful to apply a
pending migration ahead of time, or to retry after a previously failed
migration once its cause is resolved.

A development build (one the release pipeline did not version-stamp) refuses
a database it did not create rather than migrating it; pass --allow-dev-build
to migrate deliberately anyway. A release build always migrates, with or
without the flag.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		path := persistence.DefaultPath()
		ensure := persistence.EnsureCurrent
		if storageMigrateAllowDevBuild {
			ensure = persistence.EnsureCurrentAllowDevBuild
		}
		db, err := ensure(cmd.Context(), path)
		if err != nil {
			return err
		}
		defer db.Close()

		version, err := db.Version(cmd.Context())
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s is at schema version %d\n", path, version)
		return nil
	},
}

var (
	storageImportFrom     string
	storageImportDataHome string
	storageImportDryRun   bool
)

var storageImportCmd = &cobra.Command{
	Use:   "import",
	Short: "One-time import of a stopped-writer legacy data directory into SQLite",
	Long: `Reads a legacy plect data directory (state.json, the events/ tree) from
--from, builds and validates a temporary SQLite database, and — unless
--dry-run — atomically promotes it to storage.db and writes the legacy
rejection marker over state.json in the target data directory, defaulting to
$XDG_DATA_HOME/plect.

Every writer against --from must already be stopped, and --from should be a
backup copy, not the live data directory: this command validates that no
lock file it reads is still held, but it does not stop anything itself. See
docs/migrations/ for the full cutover procedure.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		dataHome := storageImportDataHome
		if dataHome == "" {
			dataHome = defaultDataHome()
		}
		report, err := legacyimport.Run(cmd.Context(), legacyimport.Options{
			SourceDir: storageImportFrom,
			DestDir:   dataHome,
			DryRun:    storageImportDryRun,
		})
		if report != nil {
			fmt.Fprintln(cmd.OutOrStdout(), report.String())
		}
		return err
	},
}

// defaultDataHome is the directory persistence.DefaultPath's storage.db
// lives in ($XDG_DATA_HOME/plect), derived rather than duplicated so the two
// never disagree about XDG resolution.
func defaultDataHome() string {
	return filepath.Dir(persistence.DefaultPath())
}

func init() {
	storageMigrateCmd.Flags().BoolVar(&storageMigrateAllowDevBuild, "allow-dev-build", false,
		"Let a development build forward-migrate a database it did not create (a release build always may)")
	storageCmd.AddCommand(storageMigrateCmd)

	storageImportCmd.Flags().StringVar(&storageImportFrom, "from", "", "Legacy data directory to import (a stopped-writer backup, not the live directory)")
	storageImportCmd.Flags().StringVar(&storageImportDataHome, "data-home", "", "Target plect data directory (default: $XDG_DATA_HOME/plect)")
	storageImportCmd.Flags().BoolVar(&storageImportDryRun, "dry-run", false, "Validate and report without promoting a database or writing the rejection marker")
	_ = storageImportCmd.MarkFlagRequired("from")
	storageCmd.AddCommand(storageImportCmd)

	rootCmd.AddCommand(storageCmd)
}
