package legacyimport

import "fmt"

// Report counts what one Run call found, even when Run also returns an
// error: a rejected import still explains itself.
type Report struct {
	Sessions                 int
	SessionsFromEventLogOnly int // present only via events/, no state.json entry
	Events                   int
	InternalBackfilled       int // events with no direction, imported as internal
	Cursors                  int
	Populations              int
	PopulationMembers        int
	UpReservations           int
	UnknownFiles             []string // paired with a non-nil Run error; see doc.go
	Promoted                 bool     // true once storage.db and the marker are both written
	DBPath                   string
	MarkerPath               string
}

func (r *Report) String() string {
	return fmt.Sprintf(
		"sessions=%d (event-log-only=%d) events=%d (internal-backfilled=%d) cursors=%d "+
			"populations=%d population-members=%d up-reservations=%d unknown-files=%d promoted=%v",
		r.Sessions, r.SessionsFromEventLogOnly, r.Events, r.InternalBackfilled, r.Cursors,
		r.Populations, r.PopulationMembers, r.UpReservations, len(r.UnknownFiles), r.Promoted,
	)
}
