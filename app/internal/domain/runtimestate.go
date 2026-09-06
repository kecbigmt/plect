package domain

import (
	"errors"
	"time"
)

// UpReservation has no TTL field: RunSetup has no deadline, so only a
// confirmed-dead holder, never elapsed time, may expire one. Parent is
// either a real session name or VirtualRootReservationParent; the
// persistence layer stores that distinction relationally (a nullable
// parent-name column plus a boolean) rather than as this sentinel string,
// translating between the two at the read/write boundary.
type UpReservation struct {
	Parent string    `json:"parent"`
	At     time.Time `json:"at"` // diagnostic only; expiry never reads it
	PID    int       `json:"pid"`
}

// VirtualRootReservationParent marks a reservation counted against the
// virtual root's own max_up_children cap (a parentless session, or one
// whose parent is itself the "root:" pseudo-parent) rather than a real
// parent session.
const VirtualRootReservationParent = "@virtual-root"

// PopulationState holds durable source generations independently of session
// state, so a successful absence can suppress stale appearances even after a
// session has been destroyed.
type PopulationState struct {
	Workflow string                       `json:"workflow"`
	Name     string                       `json:"name"`
	Members  map[string]*PopulationMember `json:"members,omitempty"`
}

type PopulationMember struct {
	ResourceID     string         `json:"resource_id"`
	Item           map[string]any `json:"item,omitempty"`
	SessionName    string         `json:"session_name,omitempty"`
	Generation     uint64         `json:"generation"`
	AcceptedAt     time.Time      `json:"accepted_at,omitzero"`
	LastAppearance time.Time      `json:"last_appearance,omitzero"`
	LastInbound    time.Time      `json:"last_inbound,omitzero"`
	Tombstoned     bool           `json:"tombstoned,omitempty"`
	PendingUp      bool           `json:"pending_up,omitempty"`
	LastDecision   string         `json:"last_decision,omitempty"`
	LastBlockers   []string       `json:"last_blockers,omitempty"`
}

// ErrUpAlreadyReserved: childName's reservation is held by another live
// process — a second concurrent `plect up` for that child, not a sibling.
var ErrUpAlreadyReserved = errors.New("state: up already reserved by a live process")
