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

var storageMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Apply any pending schema migrations to the local database",
	Long: `Runs the same migration runner every plect command, plect serve, and
plect-web already run automatically at startup, explicitly. Useful to apply a
pending migration ahead of time, or to retry after a previously failed
migration once its cause is resolved.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		path := persistence.DefaultPath()
		db, err := persistence.EnsureCurrent(cmd.Context(), path)
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
	storageCmd.AddCommand(storageMigrateCmd)
	rootCmd.AddCommand(storageCmd)
}
