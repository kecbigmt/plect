package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/app/internal/state"
)

var (
	workflowListJSON       bool
	workflowListNoHeader   bool
	workflowShowJSON       bool
	workflowPopulationJSON bool
)

var workflowCmd = &cobra.Command{
	Use:   "workflow",
	Short: "Discover available workflows",
}

var workflowListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List available workflows",
	Long: `List workflows discoverable from the cascade (global + ancestor .plect/workflows/).

Default output is a space-aligned table with ID / NAME / DESCRIPTION columns.
Use --json for a fully structured listing; --no-header switches to tab-separated
output without a header row so the result is consumable by cut/awk/etc.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("getwd: %w", err)
		}
		workflows, err := service.WorkflowList(cfg, cwd)
		if err != nil {
			return err
		}
		if workflowListJSON {
			b, err := json.MarshalIndent(workflows, "", "  ")
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(b))
			return nil
		}
		if len(workflows) == 0 {
			fmt.Fprintln(cmd.ErrOrStderr(), "No workflows found")
			return nil
		}
		return writeWorkflowList(cmd.OutOrStdout(), workflows, workflowListNoHeader)
	},
}

// writeWorkflowList renders the list result. --no-header switches from
// tabwriter space-padding to literal tab separators so the bytes are
// machine-consumable; the human form keeps the same `(no description)` /
// `(no name)` sentinels so a blank column doesn't look like a glitch.
func writeWorkflowList(out io.Writer, workflows []service.WorkflowSummary, noHeader bool) error {
	if noHeader {
		for _, wf := range workflows {
			if _, err := fmt.Fprintf(out, "%s\t%s\t%s\n", wf.ID, wf.Name, wf.Description); err != nil {
				return err
			}
		}
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tDESCRIPTION")
	for _, wf := range workflows {
		name := wf.Name
		if name == "" {
			name = "(no name)"
		}
		desc := wf.Description
		if desc == "" {
			desc = "(no description)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", wf.ID, name, desc)
	}
	return w.Flush()
}

var workflowShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Show full information about a workflow",
	Long: `Show full information about a workflow: identity, workspace provider,
inputs schema, node DAG, event channels, and populations.

Exit code is nonzero when the workflow's referenced workspace provider is
declared but fails to load — the failure is still printed (or included in
--json as workspace_provider_error) alongside everything else that resolved
cleanly.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("getwd: %w", err)
		}
		detail, err := service.WorkflowShow(cfg, cwd, args[0])
		if err != nil {
			return err
		}
		if workflowShowJSON {
			b, err := json.MarshalIndent(detail, "", "  ")
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(b))
		} else if err := printWorkflowDetail(cmd.OutOrStdout(), detail); err != nil {
			return err
		}
		if detail.WorkspaceProviderError != "" {
			return fmt.Errorf("workspace provider %q failed to load: %s", detail.WorkspaceProvider, detail.WorkspaceProviderError)
		}
		return nil
	},
}

var workflowPopulationsCmd = &cobra.Command{
	Use:   "populations <workflow-id> <population-name>",
	Short: "Show a population's recorded members and their admit status",
	Long: `Show one population's recorded members: resource, session, whether it is
pending admission, and — for a member with a session — its most recent
admit outcome (reason, error, and how many admits have failed in a row
since the last one that succeeded).

Reads runtime state directly, not current config, so it still shows a
population whose config was just changed or removed out from under it.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		store := state.NewStore("")
		members, err := service.PopulationStatus(store, args[0], args[1])
		if err != nil {
			return err
		}
		if workflowPopulationJSON {
			b, err := json.MarshalIndent(members, "", "  ")
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(b))
			return nil
		}
		if len(members) == 0 {
			fmt.Fprintln(cmd.ErrOrStderr(), "No recorded members")
			return nil
		}
		return writePopulationMembers(cmd.OutOrStdout(), members)
	},
}

func writePopulationMembers(out io.Writer, members []service.PopulationMemberStatus) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "RESOURCE\tSESSION\tPENDING\tLAST_ADMIT_ERROR\tSTREAK")
	for _, m := range members {
		session := m.Session
		if session == "" {
			session = "-"
		}
		lastError := "-"
		if m.LastAdmitReason != "" {
			lastError = fmt.Sprintf("%s: %s", m.LastAdmitReason, m.LastAdmitError)
		}
		streak := "-"
		if m.ConsecutiveAdmitFailures > 0 {
			streak = fmt.Sprintf("%d", m.ConsecutiveAdmitFailures)
		}
		fmt.Fprintf(w, "%s\t%s\t%t\t%s\t%s\n", m.Resource, session, m.PendingUp, lastError, streak)
	}
	return w.Flush()
}

func printWorkflowDetail(out io.Writer, d *service.WorkflowDetail) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "ID:\t%s\n", d.ID)
	if d.Name != "" {
		fmt.Fprintf(w, "Name:\t%s\n", d.Name)
	}
	if d.Description != "" {
		fmt.Fprintf(w, "Description:\t%s\n", d.Description)
	}
	if d.WorkspaceProvider != "" {
		fmt.Fprintf(w, "Workspace provider:\t%s\n", d.WorkspaceProvider)
	}
	if !d.AutoSelect {
		fmt.Fprintln(w, "Auto-select:\tfalse")
	}
	if d.WorkspaceProviderInfo != nil {
		if d.WorkspaceProviderInfo.Match != "" {
			fmt.Fprintf(w, "Resolver:\t%s → %s\n", d.WorkspaceProviderInfo.Match, d.WorkspaceProviderInfo.Name.Source())
		}
		var hooks []string
		if d.WorkspaceProviderInfo.Setup != nil {
			hooks = append(hooks, "setup")
		}
		if d.WorkspaceProviderInfo.Cleanup != nil {
			hooks = append(hooks, "cleanup")
		}
		if len(hooks) > 0 {
			fmt.Fprintf(w, "Workspace provider hooks:\t%s (use --json for the scripts)\n", strings.Join(hooks, ", "))
		}
	}
	if d.WorkspaceProviderError != "" {
		fmt.Fprintf(w, "Workspace provider error:\t%s\n", d.WorkspaceProviderError)
	}
	if d.Tick != nil {
		if len(d.Tick.On) > 0 {
			fmt.Fprintf(w, "Tick on:\t%s\n", strings.Join(d.Tick.On, ", "))
		}
		if d.Tick.Heartbeat.Duration > 0 {
			fmt.Fprintf(w, "Tick heartbeat:\t%s\n", d.Tick.Heartbeat.Duration)
		}
	}
	if len(d.Display) > 0 {
		keys := make([]string, 0, len(d.Display))
		for k := range d.Display {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(w, "Display %s:\t%s\n", k, d.Display[k])
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}

	if len(d.InputsSchema) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Inputs:")
		writeInputsSummary(out, d.InputsSchema)
	}

	if len(d.Nodes) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Nodes (execute order):")
		writeNodesByScope(out, d.Nodes)
	}

	if len(d.Channels) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Event channels:")
		writeChannels(out, d.Channels)
	}
	if len(d.Populations) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Populations:")
		writePopulations(out, d.Populations)
	}
	return nil
}

func writePopulations(out io.Writer, populations []service.WorkflowPopulation) {
	for _, population := range populations {
		fmt.Fprintf(out, "  %s (observer %s)\n", population.Name, population.ResourceObserver)
		fmt.Fprintf(out, "    uses: %s\n", strings.Join(population.Uses, ", "))
		if population.PollEvery != "" {
			fmt.Fprintf(out, "    poll every: %s\n", population.PollEvery)
		}
		if population.ExpireAfter != "" {
			fmt.Fprintf(out, "    expire after: %s\n", population.ExpireAfter)
		}
		fmt.Fprintf(out, "    auto down: %t, auto destroy: %t\n", population.AutoDown, population.AutoDestroy)
	}
}

// writeChannels lists each [[event.channel]] with the primitive it resolves to
// and the event types it delivers, matching the indented outline of the Nodes
// section above.
func writeChannels(out io.Writer, channels []service.WorkflowChannel) {
	for _, ch := range channels {
		line := "  " + ch.Name + " (uses " + ch.Uses
		if ch.Type != "" {
			line += ", " + ch.Type
		}
		line += ")"
		fmt.Fprintln(out, line)
		if len(ch.Include) > 0 {
			fmt.Fprintf(out, "    delivers: %s\n", strings.Join(ch.Include, ", "))
		}
	}
}

// writeNodesByScope groups the topo-sorted node list by scope. Both the scope
// header and each node id are indented under the `Nodes (execute order):`
// heading so the whole listing reads as one nested outline — matching the
// `Inputs:` section above (which also uses pure indent rather than bullets).
// StreamReporter's flush-left scope headers earn their position by sharing a
// column with status icons during a run; `show` has no icons, so the indented
// form reads cleaner here. Deps are omitted on purpose; order is the signal.
func writeNodesByScope(out io.Writer, nodes []service.WorkflowNode) {
	currentScope := ""
	for _, n := range nodes {
		if n.Scope != currentScope {
			fmt.Fprintf(out, "  %s:\n", n.Scope)
			currentScope = n.Scope
		}
		line := "    " + n.ID
		if n.Uses != "" && n.Uses != n.ID {
			line += " (uses " + n.Uses + ")"
		}
		fmt.Fprintln(out, line)
	}
}

// writeInputsSummary renders the top-level `properties` of a JSON Schema as
// human-readable bullets. allOf-merged schemas (cascade overlay) are flattened
// across their constituents so the reader sees a single list.
func writeInputsSummary(out io.Writer, schema map[string]any) {
	keys := collectSchemaKeys(schema)
	if len(keys) == 0 {
		fmt.Fprintln(out, "  (no inputs)")
		return
	}
	required := collectRequired(schema)
	props := collectProperties(schema)
	sort.Strings(keys)
	for _, k := range keys {
		prop, _ := props[k].(map[string]any)
		typ := stringValue(prop, "type")
		if typ == "" {
			typ = "any"
		}
		flag := "optional"
		if required[k] {
			flag = "required"
		}
		fmt.Fprintf(out, "  %s (%s, %s)\n", k, typ, flag)
		if desc := stringValue(prop, "description"); desc != "" {
			fmt.Fprintf(out, "    %s\n", desc)
		}
		if exs := stringListValue(prop, "examples"); len(exs) > 0 {
			fmt.Fprintf(out, "    Examples: %s\n", strings.Join(exs, ", "))
		}
		if enum := stringListValue(prop, "enum"); len(enum) > 0 {
			fmt.Fprintf(out, "    Allowed: %s\n", strings.Join(enum, ", "))
		}
	}
}

// collectProperties walks an inputs_schema (including allOf cascades) and
// returns the union of `properties` maps. Later layers don't override earlier
// ones — we only flatten so the reader sees every declared key.
func collectProperties(schema map[string]any) map[string]any {
	out := map[string]any{}
	for _, layer := range schemaLayers(schema) {
		if props, ok := layer["properties"].(map[string]any); ok {
			for k, v := range props {
				if _, exists := out[k]; !exists {
					out[k] = v
				}
			}
		}
	}
	return out
}

func collectRequired(schema map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, layer := range schemaLayers(schema) {
		if req, ok := layer["required"].([]any); ok {
			for _, r := range req {
				if s, ok := r.(string); ok {
					out[s] = true
				}
			}
		}
	}
	return out
}

func collectSchemaKeys(schema map[string]any) []string {
	props := collectProperties(schema)
	out := make([]string, 0, len(props))
	for k := range props {
		out = append(out, k)
	}
	return out
}

// schemaLayers returns the schema itself plus any allOf entries (cascade
// overlay flattens layers as `allOf` in combineInputsSchemas).
func schemaLayers(schema map[string]any) []map[string]any {
	out := []map[string]any{schema}
	if all, ok := schema["allOf"].([]any); ok {
		for _, entry := range all {
			if m, ok := entry.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}
	return out
}

func stringValue(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func stringListValue(m map[string]any, key string) []string {
	if m == nil {
		return nil
	}
	raw, ok := m[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		switch x := v.(type) {
		case string:
			out = append(out, x)
		default:
			out = append(out, fmt.Sprintf("%v", x))
		}
	}
	return out
}

func init() {
	workflowListCmd.Flags().BoolVar(&workflowListJSON, "json", false, "Output as JSON array")
	workflowListCmd.Flags().BoolVar(&workflowListNoHeader, "no-header", false, "Omit the header row")

	workflowShowCmd.Flags().BoolVar(&workflowShowJSON, "json", false, "Output as JSON")
	workflowPopulationsCmd.Flags().BoolVar(&workflowPopulationJSON, "json", false, "Output as JSON array")

	workflowCmd.AddCommand(workflowListCmd)
	workflowCmd.AddCommand(workflowShowCmd)
	workflowCmd.AddCommand(workflowPopulationsCmd)
	rootCmd.AddCommand(workflowCmd)
}
