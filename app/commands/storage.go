package commands

import (
	"fmt"

	"github.com/spf13/cobra"

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

func init() {
	storageMigrateCmd.Flags().BoolVar(&storageMigrateAllowDevBuild, "allow-dev-build", false,
		"Let a development build forward-migrate a database it did not create (a release build always may)")
	storageCmd.AddCommand(storageMigrateCmd)
	rootCmd.AddCommand(storageCmd)
}
