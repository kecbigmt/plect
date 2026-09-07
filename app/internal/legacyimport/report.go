package legacyimport

import "fmt"

// Report counts what one Run call found, even when Run also returns an
// error: a rejected import still explains itself.
type Report struct {
	Sessions int
	// SkippedEventOnly is visibility only: an events/<name> directory with
	// no state.json entry adds nothing to Sessions or Events, so an
	// operator would otherwise have no way to see it was left behind.
	SkippedEventOnly   int
	Events             int
	InternalBackfilled int // see SessionLog.InternalBackfilled
	Cursors            int
	Populations        int
	PopulationMembers  int
	UpReservations     int
	UnknownFiles       []string // paired with a non-nil Run error; see doc.go
	Promoted           bool     // true once storage.db and the marker are both written
	DBPath             string
	MarkerPath         string
}

func (r *Report) String() string {
	return fmt.Sprintf(
		"sessions=%d skipped-event-only=%d events=%d (internal-backfilled=%d) cursors=%d "+
			"populations=%d population-members=%d up-reservations=%d unknown-files=%d promoted=%v",
		r.Sessions, r.SkippedEventOnly, r.Events, r.InternalBackfilled, r.Cursors,
		r.Populations, r.PopulationMembers, r.UpReservations, len(r.UnknownFiles), r.Promoted,
	)
}
