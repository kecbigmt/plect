package commands

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/app/internal/task"
	"github.com/kecbigmt/plecture/contracts/event"
)

var (
	statusJSON    bool
	statusRefresh bool
	statusFull    bool
)

var statusCmd = &cobra.Command{
	Use:   "status <resource-id|session>",
	Short: "Report a session's identity, runtime, work, and flow facts",
	Long: `Renders four layers of session state, purely from persisted facts — no
provider-specific interpretation:

  identity  resource id / workflow / tag / tree position (parent, children)
  runtime   whether the session is actually alive: run-scoped task state,
            runtime liveness, workdir existence
  work      each task instance's outputs (dynamic and mutable alike),
            done_when evaluation, heartbeat budget, and chain plan — the same
            facts "plect tick" acts on and "plect check" used to report
  flow      the most recent inbound/outbound events

Reporting reads what was last observed, and says when that was. Pass
--refresh to observe each resource before reporting, so the facts shown are
the ones current at the moment of the call.

No provider-specific field exists here — a PR's review decision or checks
status appears under "work" only when the observer the task document declares
publishes it. "plect ls" still carries the workflow's own display status
line.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if statusFull && !statusJSON {
			return fmt.Errorf("--full is only supported with --json")
		}

		cfg, err := config.Load()
		if err != nil {
			return err
		}
		store := state.NewStore("")

		if statusRefresh {
			if _, err := service.ObserveSessionResources(cfg, store, args[0]); err != nil {
				return fmt.Errorf("observing resources failed: %w", err)
			}
		}

		result, err := service.Status(cfg, store, args[0])
		if err != nil {
			return err
		}

		if statusJSON {
			// Warnings stay out of stderr in --json mode: a downstream script
			// that captures this command's output with `2>&1` feeds it
			// straight to jq, and a warning line ahead of the JSON breaks
			// that parse.
			var payload any = result
			if !statusFull {
				payload = service.Summarize(result)
			}
			b, err := json.MarshalIndent(payload, "", "  ")
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(b))
			return nil
		}

		for _, w := range result.Warnings {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", w)
		}
		renderStatus(cmd, result)
		return nil
	},
}

func renderStatus(cmd *cobra.Command, result *service.StatusResult) {
	out := cmd.OutOrStdout()
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)

	id := result.Identity
	fmt.Fprintf(w, "Session:\t%s\n", id.SessionName)
	if result.Destroyed {
		fmt.Fprintf(w, "Status:\tdestroyed (tombstone)\n")
		fmt.Fprintf(w, "Resource:\t%s\n", id.ResourceID)
		fmt.Fprintf(w, "Created:\t%s\n", id.CreatedAt.Format("2006-01-02 15:04:05"))
		fmt.Fprintf(w, "Destroyed:\t%s\n", result.DestroyedAt.Format("2006-01-02 15:04:05"))
		w.Flush()
		renderStatusWork(out, result.Work, true)
		return
	}

	fmt.Fprintf(w, "Resource:\t%s\n", id.ResourceID)
	if id.Title != "" {
		fmt.Fprintf(w, "Title:\t%s\n", id.Title)
	}
	switch {
	case id.Workflow != "" && id.Tag != "" && id.Tag != id.Workflow:
		fmt.Fprintf(w, "Workflow:\t%s (tag: %s)\n", id.Workflow, id.Tag)
	case id.Workflow != "":
		fmt.Fprintf(w, "Workflow:\t%s\n", id.Workflow)
	case id.Tag != "":
		fmt.Fprintf(w, "Tag:\t%s\n", id.Tag)
	}
	if id.ParentSession != "" {
		fmt.Fprintf(w, "Parent:\t%s\n", id.ParentSession)
	}

	rt := result.Runtime
	fmt.Fprintf(w, "Run:\t%s\n", formatRunLine(rt))
	fmt.Fprintf(w, "Health:\t%s\n", formatHealthLine(rt))
	if rt.Message != nil && rt.Message.Text != "" {
		fmt.Fprintf(w, "Message:\t%s (%s)\n", rt.Message.Text, formatLastActive(rt.Message.UpdatedAt))
	} else {
		fmt.Fprintf(w, "Message:\t-\n")
	}
	w.Flush()

	if len(result.Flow.Events) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Events")
		for _, e := range result.Flow.Events {
			arrow := "→"
			if e.Direction == event.Inbound {
				arrow = "←"
			}
			fmt.Fprintf(out, "  %s %s %s\t%s\n", e.Time.Format("15:04"), arrow, e.Type, e.Summary)
		}
	}

	renderDoneWhenSections(out, result.Work)
}

// renderStatusWork prints one "Task <name> <status>" line per produced
// instance, with a done expansion only for instances that declare a
// done_when — the unified display rule applies regardless of task origin
// (run / node / @workflow / initial / runtime-added), so there is no
// scope/case-splitting by origin here.
func renderStatusWork(out interface{ Write([]byte) (int, error) }, work []service.StatusTask, persisted bool) {
	for i, t := range work {
		if i > 0 {
			fmt.Fprintln(out)
		}
		line := "Task " + taskDisplayName(t.Name, t.Instance)
		if t.Status != "" {
			line += " " + t.Status
		}
		if t.DoneWhen != nil && (t.HeartbeatBudget > 0 || t.HeartbeatTicks > 0) {
			line += fmt.Sprintf("   heartbeat budget %d/%d", t.HeartbeatTicks, t.HeartbeatBudget)
		}
		fmt.Fprintln(out, line)
		if t.DoneWhen != nil {
			fmt.Fprintln(out, "  done: "+formatDoneWhen(t.DoneWhen))
		}
		if persisted && t.PersistedDoneWhen != nil {
			fmt.Fprintf(out, "  done_when: %s (heartbeat ticks %d)\n", t.PersistedDoneWhen.LastAction, t.PersistedDoneWhen.HeartbeatTicks)
		}
		for _, c := range t.Chains {
			fmt.Fprintf(out, "  chain %s: %s\n", c.ChainID, statusChainSpawnStatus(c))
		}
		if t.Observed != nil {
			line := "  observed " + formatObservedAge(t.Observed.At)
			if pairs := sortedPairs(t.Observed.State); pairs != "" {
				line += ": " + pairs
			}
			fmt.Fprintln(out, line)
		}
		if pairs := sortedPairs(t.State); pairs != "" {
			fmt.Fprintf(out, "  state: %s\n", pairs)
		}
		if len(t.Outputs) > 0 {
			keys := make([]string, 0, len(t.Outputs))
			for k := range t.Outputs {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			pairs := make([]string, 0, len(keys))
			for _, k := range keys {
				pairs = append(pairs, fmt.Sprintf("%s=%v", k, t.Outputs[k]))
			}
			fmt.Fprintf(out, "  outputs: %s\n", strings.Join(pairs, " "))
		}
	}
}

// formatObservedAge says how old an observation is, so a stale fact is
// visible rather than silent: reporting renders the last observation, and
// only `--refresh` takes a new one.
func formatObservedAge(at time.Time) string {
	if at.IsZero() {
		return "at an unrecorded time"
	}
	return fmt.Sprintf("%s ago (%s)", formatLastActive(at), at.Format(time.RFC3339))
}

func sortedPairs(values map[string]any) string {
	if len(values) == 0 {
		return ""
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, fmt.Sprintf("%s=%v", k, values[k]))
	}
	return strings.Join(pairs, " ")
}

// renderDoneWhenSections prints one "Done when (<instance>)" block per task
// instance that declares a done_when. Instances without one are absent from
// this section entirely, not elided — their lifecycle status is irrelevant to
// the done_when/chain decision this section reports (full task lifecycle and
// outputs remain available via --json --full).
func renderDoneWhenSections(out interface{ Write([]byte) (int, error) }, work []service.StatusTask) {
	for _, t := range work {
		if t.DoneWhen == nil {
			continue
		}
		fmt.Fprintln(out)
		fmt.Fprintf(out, "Done when (%s)\n", taskDisplayName(t.Name, t.Instance))
		if budget := service.HeartbeatBudgetString(t.HeartbeatTicks, t.HeartbeatBudget); budget != "" {
			fmt.Fprintf(out, "  heartbeat budget: %s\n", budget)
		}
		if t.Action != "" {
			fmt.Fprintf(out, "  action: %s\n", t.Action)
		}
		if len(t.DoneWhen.Leaves) > 0 {
			fmt.Fprintln(out, "  conditions")
			for _, leaf := range t.DoneWhen.Leaves {
				fmt.Fprintf(out, "    %s\n", formatDoneWhenLeafLine(leaf))
			}
		}
		if len(t.Chains) > 0 {
			fmt.Fprintln(out, "  chains")
			for _, c := range t.Chains {
				fmt.Fprintf(out, "    %s: %s\n", c.ChainID, statusChainDetailLine(c))
			}
		}
	}
}

// formatDoneWhenLeafLine renders one done_when leaf as a single condition
// line: a check leaf as its referenced output and observed value, a judge
// leaf as its id, verdict@revision, and the expression it's judging.
func formatDoneWhenLeafLine(leaf task.DoneLeafResult) string {
	sym := doneLeafSymbol(leaf.Status)
	if leaf.Kind == "judge" {
		id := leaf.ID
		if id == "" {
			id = "judge"
		}
		rev := leaf.CurrentRevision
		if rev == "" {
			rev = leaf.Revision
		}
		return fmt.Sprintf("%s %s  %s@current:%s  (%s)", sym, id, leaf.Status, shortRevision(rev), leaf.Expr)
	}
	v := leaf.Value
	if !leaf.Observed {
		v = "?"
	}
	return fmt.Sprintf("%s %s=%s", sym, leaf.Output, v)
}

func doneLeafSymbol(s task.DoneStatus) string {
	switch s {
	case task.DoneSatisfied:
		return "✓"
	case task.DoneUnsatisfied:
		return "✗"
	default:
		return "⋯"
	}
}

// shortRevision truncates an opaque revision string to a display-friendly
// length — the full value stays available via --json --full.
func shortRevision(rev string) string {
	const n = 7
	if len(rev) > n {
		return rev[:n]
	}
	return rev
}

// statusChainDetailLine renders one chain's evaluation with enough detail to
// tell an operator whether anything will act automatically, and why not:
// "already-active (<target>)", "eligible (<target>)", or "not-fired" with the
// blocked reason when one is recorded. status never spawns anything — it is
// a dry-run plan — so "fired" is reserved for a session that actually
// exists (already-active); a predicate that merely holds, including one a
// parent's max_up_children cap has been refusing every tick, is eligible
// until a real tick spawns it.
func statusChainDetailLine(c service.StatusChain) string {
	switch {
	case c.AlreadyActive:
		return fmt.Sprintf("already-active (%s)", c.TargetSession)
	case c.Fired:
		return fmt.Sprintf("eligible (%s)", c.TargetSession)
	}
	detail := ""
	switch c.BlockedReason {
	case "when_unmet":
		detail = " (when not satisfied)"
	case "outputs_missing":
		if len(c.MissingOutputs) > 0 {
			detail = fmt.Sprintf(" (missing outputs: %s)", strings.Join(c.MissingOutputs, ", "))
		} else {
			detail = " (missing outputs)"
		}
	case "invalid_bindings":
		detail = " (invalid input bindings)"
	case "workflow_unresolved":
		detail = " (workflow unresolved)"
	}
	return "not-fired" + detail
}

// formatRunLine renders the Run fact with a parenthetical hint of why: which
// run-scoped task instance is produced, if any.
func formatRunLine(rt service.StatusRuntime) string {
	if rt.Run != domain.RunUp {
		return string(rt.Run)
	}
	for _, t := range rt.Tasks {
		if t.Status == "produced" {
			return fmt.Sprintf("%s (%s)", rt.Run, t.Status)
		}
	}
	return string(rt.Run)
}

// formatHealthLine renders the Health fact; a run=down session has nothing
// evaluated (the health cycle only probes sessions with a produced run-scoped
// task), so it shows "-" rather than a stale or misleading health value.
func formatHealthLine(rt service.StatusRuntime) string {
	if rt.Run != domain.RunUp {
		return "-"
	}
	parts := []string{string(rt.Health)}
	if !rt.LastCheckedAt.IsZero() {
		parts = append(parts, "last_checked_at "+rt.LastCheckedAt.Format("2006-01-02 15:04:05"))
	}
	if !rt.LastActivityAt.IsZero() {
		parts = append(parts, "last_activity_at "+rt.LastActivityAt.Format("2006-01-02 15:04:05"))
	}
	return strings.Join(parts, "   ")
}

// statusChainSpawnStatus renders a StatusChain the same way chainSpawnStatus
// (tick.go) renders a ChainSpawn — status never spawns, so Spawned is always
// false on the constructed value.
func statusChainSpawnStatus(c service.StatusChain) string {
	return chainSpawnStatus(service.ChainSpawn{
		Fired:         c.Fired,
		BlockedReason: c.BlockedReason,
		AlreadyActive: c.AlreadyActive,
		TargetSession: c.TargetSession,
	})
}

func init() {
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "Output as JSON")
	statusCmd.Flags().BoolVar(&statusRefresh, "refresh", false, "Observe each instance's resource before reporting, so the facts shown are current")
	statusCmd.Flags().BoolVar(&statusFull, "full", false, "With --json, output the full report (all task instances, unfiltered outputs, runtime/flow detail) instead of the default done_when-focused summary")
	rootCmd.AddCommand(statusCmd)
}
