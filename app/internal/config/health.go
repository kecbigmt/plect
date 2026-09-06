package config

import (
	"fmt"

	"github.com/kecbigmt/plecture/app/internal/lang"
)

// HealthConfig is an effect's `[health]` table: how this effect's health is
// determined. The two probes are independent — an effect may declare either,
// both, or (by omitting the table) neither.
//
// Alive is the liveness probe: exit-code semantics, zero means the execution
// surface is present. It may instead be a noop action, which declares that
// this effect's produced record is deliberately never re-observed — a
// systemd RemainAfterExit=-style choice, not the absence of one. Activity is
// the activity probe: it writes a JSON activity envelope on stdout whose
// opaque fingerprint core compares across evaluations. A readiness-style
// third probe is deliberately absent — health answers "is this surface
// present and moving", not "may traffic be sent".
type HealthConfig struct {
	Alive    *lang.Action
	Activity *lang.Action
}

// Validate rejects a `[health]` table that declares nothing. Unlike
// `[terminal]`, the members carry no all-or-nothing obligation on each other:
// a process effect may have a meaningful liveness probe and no activity to
// fingerprint, and a fingerprint-only effect (no process of its own to probe)
// is equally legitimate. A bare header is still an error rather than an inert
// declaration, because the only reason to write `[health]` is to declare a
// probe. A nil receiver (no table at all) is valid.
func (h *HealthConfig) Validate() error {
	if h == nil {
		return nil
	}
	if h.Alive == nil && h.Activity == nil {
		return fmt.Errorf("[health] table declares no probe (want alive, activity, or both)")
	}
	return nil
}

// AliveProbe returns the declared liveness probe, or nil when none is
// declared. Nil-safe so callers holding an optional table need no guard.
func (h *HealthConfig) AliveProbe() *lang.Action {
	if h == nil {
		return nil
	}
	return h.Alive
}

// ActivityProbe returns the declared activity probe, or nil when none is
// declared. Nil-safe, mirroring AliveProbe.
func (h *HealthConfig) ActivityProbe() *lang.Action {
	if h == nil {
		return nil
	}
	return h.Activity
}
