package legacyimport

import "fmt"

// Report counts what one Run call read and imported, whether or not it
// ultimately promoted a database — a caller can inspect it even when Run
// returns an error, so a rejected import still explains what it found.
type Report struct {
	Sessions                 int
	SessionsFromEventLogOnly int // present only via events/, no state.json entry
	Events                   int
	InternalBackfilled       int // events with no direction, imported as internal
	Cursors                  int
	Populations              int
	PopulationMembers        int
	UpReservations           int
	// UnknownFiles are regular files found inside a legacy session directory
	// that this package does not recognize; a non-empty slice is always
	// paired with a non-nil Run error (see doc.go).
	UnknownFiles []string
	// Promoted is true once the built database replaced DBPath and (unless
	// DryRun) the legacy rejection marker was written at MarkerPath.
	Promoted   bool
	DBPath     string
	MarkerPath string
}

func (r *Report) String() string {
	return fmt.Sprintf(
		"sessions=%d (event-log-only=%d) events=%d (internal-backfilled=%d) cursors=%d "+
			"populations=%d population-members=%d up-reservations=%d unknown-files=%d promoted=%v",
		r.Sessions, r.SessionsFromEventLogOnly, r.Events, r.InternalBackfilled, r.Cursors,
		r.Populations, r.PopulationMembers, r.UpReservations, len(r.UnknownFiles), r.Promoted,
	)
}
