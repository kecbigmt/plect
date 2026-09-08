package commands

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/app/internal/state"
)

var (
	upTag        string
	upWorkflow   string
	upInputs     string
	upInputsFile string
	upTask       string
	upParent     string
	upDetach     bool
	upRecreate   bool
)

var upCmd = &cobra.Command{
	Use:   "up <resource-id|session>",
	Short: "Run run-scoped tasks for a session",
	Long: `Run setup commands for all run-scoped tasks declared in config.toml,
in dependency-respecting order. Outputs from session-scoped tasks (already
set up during auto-create) are available to run-scoped tasks via {{.Tasks.<id>.<key>}}.

After setup, docker-compose-style: on a TTY 'up' hands off to the workflow's
attach target by replacing the plect process via syscall.Exec.
Pass -d/--detach to skip the hand-off and return after setup, the way 'up'
behaved before. Non-TTY stdout (MCP, cron, scripts) is treated as --detach
automatically; the MCP plect_up tool is always detached. Workflows without an
attach target return after setup regardless of TTY.

--tag selects the session's session-identity label. Omitted, it defaults to
the workflow id, so repeated 'plect up <resource-id> --workflow X' calls
converge on the same session. Pass --tag to
resolve a specifically-labelled session (e.g. 'plect up <resource-id> --tag X'
reuses that tagged session). --tag is only valid with a
resource identifier, not a bare session name (the name already encodes the tag).

--inputs / --inputs-file forward session inputs to the auto-create path when
the session does not yet exist; passing them against an already-created
session returns an error (session inputs are set once, at create time).

--task <id> is shorthand for --inputs '{"task":"<id>"}' on the auto-create
path. Workflows that require a task reject auto-create without it; pass
--task none for an ad-hoc session with no initial instruction.

--force-recreate rebuilds the session runtime for an existing session while
keeping its durable identity and event log. It cleans and forgets workflow
node state, dynamic task instances, environment state, and runtime observation
state so the next setup cannot resume from .Prev, then runs setup again.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		inputs, err := resolveInputsFlags(upInputs, upInputsFile)
		if err != nil {
			return err
		}
		inputs, err = service.MergeTaskInput(inputs, upTask)
		if err != nil {
			return err
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		store := state.NewStore("")
		result, err := service.Up(cfg, store, service.UpParams{
			Identifier:    args[0],
			Tag:           upTag,
			Workflow:      upWorkflow,
			Inputs:        inputs,
			ParentSession: upParent,
			Observer:      newTaskObserver(cfg),
			ForceRecreate: upRecreate,
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Session: %s\n", result.SessionName)
		if result.LifecycleConfigurationWarning != "" {
			fmt.Fprintf(os.Stderr, "Warning: %s\n", result.LifecycleConfigurationWarning)
		}

		if upDetach || !stdoutIsTerminal() {
			return nil
		}
		return autoAttach(cfg, store, result.SessionName)
	},
}

// autoAttach resolves the workflow's attach target and hands the TTY over via
// syscall.Exec, mirroring `plect attach`. A workflow without an attach target
// returns nil so `up` exits cleanly — the no-attach case is not an error,
// it's just the detached path.
func autoAttach(cfg *config.Config, store *state.Store, sessionName string) error {
	att, err := service.Attach(cfg, store, service.AttachParams{Identifier: sessionName})
	if err != nil {
		var svcErr *service.Error
		if errors.As(err, &svcErr) && svcErr.Code == service.ErrNotAttachable {
			return nil
		}
		return err
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		return fmt.Errorf("bash not found in PATH: %w", err)
	}
	return syscall.Exec(bash, []string{"bash", "-c", att.Command}, os.Environ())
}

// stdoutIsTerminal reports whether os.Stdout is a TTY. Used by `up` to
// suppress auto-attach in non-interactive environments (MCP, cron, pipes).
func stdoutIsTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func init() {
	upCmd.Flags().StringVar(&upTag, "tag", "", "Session-identity label of the session to resolve/auto-create (resource-identifier only). Defaults to the workflow id.")
	upCmd.Flags().StringVarP(&upWorkflow, "workflow", "w", "", "Workflow id (auto-create path only; must match session's frozen workflow)")
	upCmd.Flags().StringVar(&upInputs, "inputs", "", "Session inputs as a JSON object string (auto-create path only)")
	upCmd.Flags().StringVar(&upInputsFile, "inputs-file", "", "Path to a JSON file containing the session inputs object (auto-create path only)")
	upCmd.Flags().StringVar(&upTask, "task", "", "Shorthand for --inputs '{\"task\":\"<id>\"}' (auto-create path only). Pass \"none\" for no initial task.")
	upCmd.Flags().StringVar(&upParent, "parent", "", "Parent session name for auto-created sessions, or \"root:<session>\" to join that (possibly parentless) session's siblings; falls back to $PLECT_SESSION_NAME when it names an existing session")
	upCmd.Flags().BoolVarP(&upDetach, "detach", "d", false, "Return after setup instead of attaching (docker-compose-style up -d)")
	upCmd.Flags().BoolVar(&upRecreate, "force-recreate", false, "Rebuild the session runtime instead of resuming existing task outputs")
	rootCmd.AddCommand(upCmd)
}
