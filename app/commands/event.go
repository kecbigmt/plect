package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/contracts/event"
)

var (
	eventJSON      bool
	eventSince     int64
	eventCursor    string
	eventOrder     string
	eventTypes     []string
	eventSource    string
	eventDirection string
	eventLimit     int
	eventSubtree   string

	evPubType    string
	evPubSource  string
	evPubDir     string
	evPubSummary string
	evPubBody    string
	evPubMeta    []string
)

var eventCmd = &cobra.Command{
	Use:   "event",
	Short: "Inspect and publish session events",
}

var eventListCmd = &cobra.Command{
	Use:   "list [url|session]",
	Short: "List events for a session or its subtree (--subtree)",
	Long: `List events for one session; with --subtree <root> the session tree rooted
there (root + descendants) merged in time order — the canonical cross-session
scope. The session argument and --subtree are mutually exclusive: a
cross-session view spans sessions, so it takes no single session argument.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store := state.NewStore("")
		order, err := event.NormalizeOrder(eventOrder)
		if err != nil {
			return err
		}
		if err := validateScopeArgs(args); err != nil {
			return err
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		params := service.EventPageParams{
			Order:  order,
			Cursor: eventCursor,
			Filter: buildEventFilter(),
		}
		var page service.EventPageResult
		switch {
		case eventSubtree != "":
			page, err = service.EventPageSubtree(cfg, store, eventSubtree, params)
		default:
			page, err = service.EventPage(cfg, store, args[0], params)
		}
		if err != nil {
			return err
		}
		if eventJSON {
			out := map[string]any{"events": page.Events}
			if page.NextCursor != "" {
				out["next_cursor"] = page.NextCursor
			}
			return printJSON(out)
		}
		printEventTable(cmd, page.Events)
		return nil
	},
}

var eventShowCmd = &cobra.Command{
	Use:   "show <url|session> <event-id>",
	Short: "Show a single event by id",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		store := state.NewStore("")
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		ev, err := service.EventShow(cfg, store, args[0], args[1])
		if err != nil {
			return err
		}
		return printJSON(ev)
	},
}

var eventPublishCmd = &cobra.Command{
	Use:   "publish <url|session>",
	Short: "Publish an event to a session",
	Long:  "Publish an event to a session. The target session must already exist; publishing to an unknown name errors rather than creating one.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		store := state.NewStore("")
		meta, err := parseMeta(evPubMeta)
		if err != nil {
			return err
		}
		ev, err := service.EventPublish(cfg, store, args[0], service.EventPublishParams{
			Type:      evPubType,
			Source:    evPubSource,
			Direction: event.Direction(evPubDir),
			Summary:   evPubSummary,
			Body:      evPubBody,
			Metadata:  meta,
		})
		if err != nil {
			return err
		}
		if eventJSON {
			return printJSON(ev)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s\n", ev.ID)
		return nil
	},
}

var eventTailCmd = &cobra.Command{
	Use:   "tail [url|session]",
	Short: "Follow events for a session or its subtree (--subtree)",
	Long: `Follow events for one session; with --subtree <root> the session tree rooted
there (root + descendants), children spawned later included. The session
argument and --subtree are mutually exclusive.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store := state.NewStore("")
		f := buildEventFilter()
		if err := validateScopeArgs(args); err != nil {
			return err
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()

		out := cmd.OutOrStdout()
		emit := func(ev event.Event) {
			if eventJSON {
				b, _ := json.Marshal(ev)
				fmt.Fprintln(out, string(b))
				return
			}
			fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", ev.Time.Format("15:04:05"), ev.Type, ev.Source, ev.Summary)
		}
		switch {
		case eventSubtree != "":
			err = service.EventTailSubtree(ctx, cfg, store, eventSubtree, f, emit)
		default:
			err = service.EventTail(ctx, cfg, store, args[0], eventSince, f, emit)
		}
		if err == context.Canceled {
			return nil
		}
		return err
	},
}

// validateScopeArgs enforces that the event scope is unambiguous: a session
// argument and --subtree name different scopes, so at most one may be set. A
// cross-session view (--subtree) spans sessions and takes no session
// argument; the single-session view requires exactly one.
func validateScopeArgs(args []string) error {
	if eventSubtree != "" {
		if len(args) > 0 {
			return fmt.Errorf("a cross-session view spans sessions; do not also pass a session argument (%q)", args[0])
		}
		return nil
	}
	if len(args) != 1 {
		return fmt.Errorf("a session (url or name) is required, or use --subtree <root> for a cross-session view")
	}
	return nil
}

func buildEventFilter() event.Filter {
	return event.Filter{
		Types:     splitTypesArg(eventTypes),
		Sources:   event.SplitCSV(eventSource),
		Direction: event.Direction(eventDirection),
		Limit:     eventLimit,
	}
}

// splitTypesArg flattens repeatable --type flags, each of which may itself be
// a comma-separated list, so "--type a,b" and "--type a --type b" behave the
// same and match the MCP plect_event_list "types" param's comma semantics.
func splitTypesArg(vals []string) []string {
	var out []string
	for _, v := range vals {
		out = append(out, event.SplitCSV(v)...)
	}
	return out
}

func printEventTable(cmd *cobra.Command, evs []event.Event) {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TIME\tTYPE\tSOURCE\tDELIVERY\tSUMMARY\tID")
	for _, ev := range evs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			ev.Time.Format("2006-01-02 15:04:05"), ev.Type, ev.Source, deliveryLabel(ev), ev.Summary, ev.ID)
	}
	w.Flush()
}

// deliveryLabel derives the DELIVERY column display from ev.Type rather than
// a stored field: a terminal signal is pushed one hop into its receiver's
// own log by the terminal-event-propagation ADR, and that prefix is the only
// fact distinguishing it from an ordinary pull-only progress event.
func deliveryLabel(ev event.Event) string {
	if strings.HasPrefix(ev.Type, event.TypeTerminalPrefix) {
		return "push"
	}
	return "pull"
}

func printJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func parseMeta(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	m := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			return nil, fmt.Errorf("invalid --meta format %q: expected key=value", p)
		}
		m[k] = v
	}
	return m, nil
}

func init() {
	eventListCmd.Flags().BoolVar(&eventJSON, "json", false, "Output as JSON")
	eventListCmd.Flags().StringVar(&eventOrder, "order", "asc", "List order: asc (oldest first, paginates) or desc (newest N, no jq needed)")
	eventListCmd.Flags().StringVar(&eventCursor, "cursor", "", "Opaque pagination token from a prior page's next_cursor (asc only)")
	eventListCmd.Flags().StringArrayVar(&eventTypes, "type", nil, "Filter by type glob (repeatable and/or comma-separated, e.g. widget.*)")
	eventListCmd.Flags().StringVar(&eventSource, "source", "", "Filter by source (comma-separated)")
	eventListCmd.Flags().StringVar(&eventDirection, "direction", "", "Filter by direction (inbound|outbound|internal)")
	eventListCmd.Flags().IntVar(&eventLimit, "limit", 0, "Max events to return (0 = all)")
	eventListCmd.Flags().StringVar(&eventSubtree, "subtree", "", "Cross-session view: list events for the session tree rooted at this url|session (root + descendants), in time order (no session arg)")

	eventPublishCmd.Flags().StringVar(&evPubType, "type", "", "Event type (required, e.g. user.emit)")
	eventPublishCmd.Flags().StringVar(&evPubSource, "source", "", "Event source (default: cli)")
	eventPublishCmd.Flags().StringVar(&evPubDir, "direction", "", "Direction (inbound|outbound|internal)")
	eventPublishCmd.Flags().StringVar(&evPubSummary, "summary", "", "One-line summary")
	eventPublishCmd.Flags().StringVar(&evPubBody, "body", "", "Full body text")
	eventPublishCmd.Flags().StringArrayVar(&evPubMeta, "meta", nil, "Metadata key=value (repeatable)")
	eventPublishCmd.Flags().BoolVar(&eventJSON, "json", false, "Output the stored event as JSON")
	eventPublishCmd.MarkFlagRequired("type")

	eventTailCmd.Flags().BoolVar(&eventJSON, "json", false, "Output as JSON lines")
	eventTailCmd.Flags().Int64Var(&eventSince, "since", 0, "Start sequence (replay cursor)")
	eventTailCmd.Flags().StringArrayVar(&eventTypes, "type", nil, "Filter by type glob (repeatable and/or comma-separated)")
	eventTailCmd.Flags().StringVar(&eventSource, "source", "", "Filter by source (comma-separated)")
	eventTailCmd.Flags().StringVar(&eventDirection, "direction", "", "Filter by direction")
	eventTailCmd.Flags().StringVar(&eventSubtree, "subtree", "", "Cross-session view: follow events for the session tree rooted at this url|session (root + descendants), later children included (no session arg)")

	eventCmd.AddCommand(eventListCmd, eventShowCmd, eventPublishCmd, eventTailCmd)
	rootCmd.AddCommand(eventCmd)
}
