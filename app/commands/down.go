package commands

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/app/internal/state"
)

var downCmd = &cobra.Command{
	Use:   "down <resource-id|session>",
	Short: "Run run-scoped cleanup for a session",
	Long: `Run cleanup commands for all run-scoped tasks in reverse dependency order.
session-scoped tasks are preserved.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		store := state.NewStore("")
		result, err := service.Down(cfg, store, service.DownParams{
			Identifier: args[0],
			Observer:   newTaskObserver(cfg),
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Session: %s\n", result.SessionName)
		if result.LifecycleConfigurationWarning != "" {
			fmt.Fprintf(os.Stderr, "Warning: %s\n", result.LifecycleConfigurationWarning)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(downCmd)
}
