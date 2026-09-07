package commands

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/app/internal/state"
)

var (
	setOutputNode     string
	setOutputWorkflow bool
	setOutputTask     string
	setStateInstance  string
)

var stateCmd = &cobra.Command{
	Use:   "state",
	Short: "Manage session state",
}

var setMessageCmd = &cobra.Command{
	Use:   "set-message <session-or-url> <text>",
	Short: "Set the session's self-reported status message",
	Long: `Set or clear the session-level status message: the session's current
activity, or empty when the session is idle. plect does not interpret the
text — it is a slot for external self-reports (e.g. an agent's
turn-boundary hook).

An empty string clears the message.

Example:
  plect state set-message session-1 "running tests"
  plect state set-message session-1 ""`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		store := state.NewStore("")

		if err := service.SetMessage(cfg, store, args[0], args[1]); err != nil {
			return err
		}

		fmt.Fprintf(cmd.ErrOrStderr(), "Message set for %s\n", args[0])
		return nil
	},
}

var setOutputCmd = &cobra.Command{
	Use:   "set-output <session-or-url> <json>",
	Short: "Merge values into a produced task's outputs",
	Long: `Merge a JSON object into the persisted outputs of a produced task.

Targets a workflow node (--node <id>), the workflow pseudo-node (--workflow),
or a runtime task handle (--task <task-handle>, e.g. review#1).
Merge-only: keys absent from the payload are left untouched. Only keys declared
with ` + "`mutable = true`" + ` in the target's outputs schema are writable; the reserved
"workdir" key is always immutable.

Intended for external updaters (e.g. a watcher daemon refreshing observed
values like pr_state) — resources created by setup stay immutable.

An explicit value set here takes precedence over resource-derived resolution
(a dynamic output with from_resource_status = true): a later refresh only
overwrites a key it actually fetched a value for, so it never clobbers a key
its resource observation found nothing for. This is how a cross-repository
pull request gets wired to a chain's pr_url input — a work session tracked
by an issue in one repository can record the PR opened in another by calling
this command, since core resource observation only ever inspects the tracked
resource itself and never searches a second repository on the work
session's behalf.

Note: validation is against the target's CURRENT outputs schema. If a def drifts
after instantiation (a key made immutable or dropped from properties, or a new
required output added), the merge is rejected and the observed value silently
stops updating — check the watcher's logs if a done_when stalls.

Example:
  plect state set-output session-1 --node watch '{"pr_state":"merged"}'
  plect state set-output session-1 --workflow '{"title":"New title"}'
  plect state set-output session-1 --task review#1 '{"checks_status":"SUCCESS"}'`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		var payload map[string]any
		if err := json.Unmarshal([]byte(args[1]), &payload); err != nil {
			return fmt.Errorf("payload is not a JSON object: %w", err)
		}
		if payload == nil {
			return fmt.Errorf("payload is not a JSON object: got null")
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		store := state.NewStore("")
		result, err := service.SetOutput(cfg, store, service.SetOutputParams{
			Identifier: args[0],
			Node:       setOutputNode,
			Workflow:   setOutputWorkflow,
			Task:       setOutputTask,
			Outputs:    payload,
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "Updated outputs of %s for %s: %s\n", result.Target, result.SessionName, strings.Join(result.Keys, ", "))
		return nil
	},
}

var setStateCmd = &cobra.Command{
	Use:   "set <session-or-url> <json>",
	Short: "Record facts into a task instance's own state",
	Long: `Merge a JSON object into a live task instance's own state — the keys its
task document declares in ` + "`state_schema`" + ` and its completion predicate reads
as ` + "`self.state.<key>`" + `.

Merge-only: keys absent from the payload are left untouched. Every key must be
one the document declares; state carries no mutability annotation, because
state is mutable by definition.

This is how a reviewer records a verdict: the revision it was recorded
against is state the instance holds, and the completion predicate compares it
with the resource's current revision, so a later push invalidates the verdict
on its own.

Example:
  plect state set session-1 --instance review#1 '{"verdict_revision":"a1b2c3d"}'`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		var payload map[string]any
		if err := json.Unmarshal([]byte(args[1]), &payload); err != nil {
			return fmt.Errorf("payload is not a JSON object: %w", err)
		}
		if payload == nil {
			return fmt.Errorf("payload is not a JSON object: got null")
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		store := state.NewStore("")
		result, err := service.SetTaskState(cfg, store, service.SetTaskStateParams{
			Identifier: args[0],
			Instance:   setStateInstance,
			State:      payload,
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "Recorded state of %s for %s: %s\n", result.Instance, result.SessionName, strings.Join(result.Keys, ", "))
		return nil
	},
}

func init() {
	setOutputCmd.Flags().StringVar(&setOutputNode, "node", "", "Target workflow node id")
	setOutputCmd.Flags().BoolVar(&setOutputWorkflow, "workflow", false, "Target the workflow pseudo-node")
	setOutputCmd.Flags().StringVar(&setOutputTask, "task", "", "Target a produced runtime task such as review#1")
	setOutputCmd.MarkFlagsMutuallyExclusive("node", "workflow", "task")
	setOutputCmd.MarkFlagsOneRequired("node", "workflow", "task")

	setStateCmd.Flags().StringVar(&setStateInstance, "instance", "", "Target instance, such as review#1")
	setStateCmd.MarkFlagRequired("instance")

	stateCmd.AddCommand(setStateCmd)
	stateCmd.AddCommand(setMessageCmd)
	stateCmd.AddCommand(setOutputCmd)
	rootCmd.AddCommand(stateCmd)
}
