package commands

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/app/internal/state"
)

var (
	destroyForce         bool
	destroyCleanupInputs []string
)

var destroyCmd = &cobra.Command{
	Use:   "destroy <resource-id|session>",
	Short: "Tear down a session via task cleanup",
	Long: `Tear down a session in four ordered steps:

  1. run-scoped cleanup (auto-down) — skipped if no run-scoped task is
     currently in "produced" status
  2. session-scoped cleanup
  3. workspace provider cleanup
  4. state entry deletion

Default policy is fail-fast: the first cleanup error aborts the remaining
steps and the state entry is kept so you can inspect and retry. Use
--force to switch to best-effort teardown (see flag description).

--force means you accept losing unsaved local changes: it is forwarded to
workspace provider cleanup, and a workspace provider whose release step
refuses on unsaved local changes may discard them to complete the release
instead of leaving them on disk.

--input passes an opaque key=value cleanup intent through to workspace
provider cleanup (repeatable); core does not interpret it. Consult the
workspace provider's own docs for which keys it reads.

If the session has child sessions, destroy fails before any teardown step
runs: deleting it would orphan them (plect up never re-adopts an orphan). Use
--force to destroy anyway and orphan the children, or reset with
` + "`plect down`" + ` + ` + "`plect up`" + ` instead, which keeps the state entry and its
children intact.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cleanupInputs, err := parseKeyValues(destroyCleanupInputs)
		if err != nil {
			return err
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		store := state.NewStore("")
		result, err := service.Destroy(cfg, store, service.DestroyParams{
			Identifier:    args[0],
			Force:         destroyForce,
			CleanupInputs: cleanupInputs,
			Observer:      newTaskObserver(cfg),
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Destroyed session: %s\n", result.SessionName)
		if result.LifecycleConfigurationWarning != "" {
			fmt.Fprintf(os.Stderr, "Warning: %s\n", result.LifecycleConfigurationWarning)
		}
		for _, w := range result.CleanupWarnings {
			fmt.Fprintf(os.Stderr, "Warning: %s\n", w)
		}
		if result.RemovedWorkspaceDir {
			fmt.Fprintln(os.Stderr, "Removed workspace directory")
		}
		return nil
	},
}

func init() {
	destroyCmd.Flags().BoolVarP(&destroyForce, "force", "f", false, "Demote cleanup errors to warnings (recorded in CleanupWarnings) so teardown continues through workspace provider cleanup and state deletion; also forwards the force intent to cleanup hooks, where it may discard unsaved local changes in the session's workspace directory to complete the release, and proceeds when the session has child sessions, orphaning them (reported as a warning) instead of aborting")
	destroyCmd.Flags().StringArrayVar(&destroyCleanupInputs, "input", nil, "Cleanup input key=value forwarded to workspace provider cleanup, unexamined by core (repeatable)")
	rootCmd.AddCommand(destroyCmd)
}
