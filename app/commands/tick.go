package commands

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/app/internal/state"
)

var (
	tickJSON      bool
	tickNoRefresh bool
)

var tickCmd = &cobra.Command{
	Use:   "tick <url|session>",
	Short: "Advance the Goal Loop for a session (actuator)",
	Long: `The Goal Loop actuator: evaluate each done_when-bearing task instance for
a session and act on the result. Heartbeat-triggered ticks consume the
heartbeat budget; event and manual ticks can act immediately without consuming
that budget. It publishes the resulting kickback/review/escalation event, and
pushes a done/escalate terminal event to the parent exactly once per instance.
Against that same fact set it also fires
[[chains]]: a chain whose when holds and whose wired outputs are present
spawns its workflow (idempotent — an already-active target is reported, not
re-spawned; a spawn the parent's max_up_children cap refuses is reported
cap-refused, not spawned, and retried on a later tick once capacity frees).
Idempotent — safe to call repeatedly on unchanged state. Use "plect status"
to read the same evaluation (including the chain plan) without acting on it.

JSON actions are one of satisfied, wait, review_required, kick, or escalate.
Each action carries heartbeat_budget (0 means unbounded), heartbeat_ticks, a
fingerprint for unchanged-poll detection, and unmet_items with machine-readable
check/judge state.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		result, err := service.TickSession(cfg, state.NewStore(""), service.TickParams{
			SessionName: args[0],
			SkipRefresh: tickNoRefresh,
			Observer:    newTaskObserver(cfg),
		})
		if err != nil {
			return err
		}
		if tickJSON {
			b, err := json.MarshalIndent(result, "", "  ")
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(b))
			return nil
		}
		for _, action := range result.Actions {
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", action.Instance, action.Action, action.Summary)
			if action.ReviewerCommand != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "  reviewer:\t%s\n", action.ReviewerCommand)
			}
		}
		for _, sp := range result.Chains {
			fmt.Fprintf(cmd.OutOrStdout(), "chain %s\t%s\t%s\n", sp.ChainID, sp.Instance, chainSpawnStatus(sp))
		}
		return nil
	},
}

// chainSpawnStatus renders one ChainSpawn's outcome as a single word/phrase
// for "plect tick"'s tab-separated text output.
func chainSpawnStatus(sp service.ChainSpawn) string {
	switch {
	case sp.Spawned:
		return "spawned " + sp.TargetSession
	case sp.AlreadyActive:
		return "already-active " + sp.TargetSession
	case sp.CapRefused:
		return "cap-refused " + sp.TargetSession
	case sp.Fired:
		return "fired"
	default:
		return "blocked:" + sp.BlockedReason
	}
}

func init() {
	tickCmd.Flags().BoolVar(&tickJSON, "json", false, "Output actions as JSON")
	tickCmd.Flags().BoolVar(&tickNoRefresh, "no-refresh", false, "Read persisted outputs without refreshing dynamic outputs first")

	rootCmd.AddCommand(tickCmd)
}
