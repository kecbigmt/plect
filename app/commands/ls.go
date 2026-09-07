package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/app/internal/state"
)

var (
	lsJSON   bool
	lsParent string
)

var lsCmd = &cobra.Command{
	Use:   "ls",
	Short: "List sessions as a flat table: session / run / health / done_when / message",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		store := state.NewStore("")

		entries, err := service.List(cfg, store)
		if err != nil {
			return err
		}

		if lsParent != "" {
			parentName, err := service.ResolveSessionName(cfg, store, lsParent)
			if err != nil {
				return err
			}
			entries = filterByParent(entries, parentName)
		}

		if lsJSON {
			b, err := json.MarshalIndent(entries, "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(b))
			return nil
		}

		if len(entries) == 0 {
			fmt.Fprintln(os.Stderr, "No sessions found")
			return nil
		}

		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "SESSION\tRUN\tHEALTH\tDONE_WHEN\tMESSAGE")
		for _, e := range entries {
			health := string(e.Health)
			if health == "" {
				health = "-"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", e.SessionName, e.Run, health, service.DoneWhenCell(e.Tasks), formatMessage(e))
		}
		return w.Flush()
	},
}

// filterByParent keeps only the entries whose ParentSession matches
// parentName, for `plect ls --parent`.
func filterByParent(entries []service.ListEntry, parentName string) []service.ListEntry {
	filtered := entries[:0]
	for _, e := range entries {
		if e.ParentSession == parentName {
			filtered = append(filtered, e)
		}
	}
	return filtered
}

// formatMessage renders the MESSAGE column: self-reported text + age,
// truncated to keep the table scannable. "-" when unset (mirrors HEALTH's
// "-" for not-applicable).
func formatMessage(e service.ListEntry) string {
	if e.Message == nil || e.Message.Text == "" {
		return "-"
	}
	const limit = 24
	text := e.Message.Text
	if len(text) > limit {
		text = text[:limit] + "…"
	}
	return fmt.Sprintf("%s (%s)", text, formatLastActive(e.Message.UpdatedAt))
}

func formatLastActive(t time.Time) string {
	d := time.Since(t)
	if d < 0 {
		return "just now"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func init() {
	lsCmd.Flags().BoolVar(&lsJSON, "json", false, "Output as JSON")
	lsCmd.Flags().StringVar(&lsParent, "parent", "", "List only the direct children of this session")
	rootCmd.AddCommand(lsCmd)
}
