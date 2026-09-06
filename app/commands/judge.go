package commands

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/app/internal/task"
)

var (
	judgeReason   string
	judgeRevision string
	judgeSession  string
)

var judgeCmd = &cobra.Command{
	Use:   "judge",
	Short: "Record done_when judge actions",
}

func newJudgeActionCmd(use, action, short string) *cobra.Command {
	return &cobra.Command{
		Use:   use + " <session> <instance> <judge-id>",
		Short: short,
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			result, err := service.RecordJudge(cfg, state.NewStore(""), service.JudgeParams{
				SessionName:  args[0],
				Instance:     args[1],
				LeafID:       args[2],
				Action:       action,
				Reason:       judgeReason,
				Revision:     judgeRevision,
				JudgeSession: judgeSession,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Recorded %s for %s/%s judge %s at revision %s\n", result.Action, result.SessionName, result.Instance, result.LeafID, result.Revision)
			return nil
		},
	}
}

func init() {
	approveCmd := newJudgeActionCmd("approve", task.JudgeActionApprove, "Approve one done_when judge")
	requestChangesCmd := newJudgeActionCmd("request-changes", task.JudgeActionRequestChanges, "Request changes for one done_when judge")
	for _, cmd := range []*cobra.Command{approveCmd, requestChangesCmd} {
		cmd.Flags().StringVar(&judgeReason, "reason", "", "Reason for this judge action")
		cmd.Flags().StringVar(&judgeRevision, "revision", "", "Opaque revision reviewed (defaults to the instance revision output)")
		cmd.Flags().StringVar(&judgeSession, "judge-session", "", "Judge session name (defaults to $PLECT_SESSION_NAME; provenance-constrained judges require it to match the ambient judge pane)")
		cmd.MarkFlagRequired("reason")
		judgeCmd.AddCommand(cmd)
	}

	rootCmd.AddCommand(judgeCmd)
}
