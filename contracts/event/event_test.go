package event

import (
	"slices"
	"testing"
)

func TestMatchType(t *testing.T) {
	cases := []struct {
		pattern, typ string
		want         bool
	}{
		{"*", "acme.ci_status", true},
		{"acme.ci_status", "acme.ci_status", true},
		{"acme.ci_status", "acme.state", false},
		{"acme.*", "acme.ci_status", true},
		{"acme.*", "acme.", true},
		{"acme.*", "acme", false}, // prefix is "acme." — bare "acme" excluded
		{"acme.*", "widget.message", false},
		{"widget.*", "widget.reply", true},
	}
	for _, c := range cases {
		if got := MatchType(c.pattern, c.typ); got != c.want {
			t.Errorf("MatchType(%q, %q) = %v, want %v", c.pattern, c.typ, got, c.want)
		}
	}
}

func TestFilterMatch(t *testing.T) {
	ev := Event{Type: "acme.ci_status", Source: "example", Direction: Internal}

	cases := []struct {
		name string
		f    Filter
		want bool
	}{
		{"zero filter matches all", Filter{}, true},
		{"type glob hit", Filter{Types: []string{"acme.*"}}, true},
		{"type glob miss", Filter{Types: []string{"widget.*"}}, false},
		{"type multi any-hit", Filter{Types: []string{"widget.*", "acme.*"}}, true},
		{"source hit", Filter{Sources: []string{"example"}}, true},
		{"source miss", Filter{Sources: []string{"other"}}, false},
		{"direction hit", Filter{Direction: Internal}, true},
		{"direction miss", Filter{Direction: Inbound}, false},
		{"combined hit", Filter{Types: []string{"acme.*"}, Sources: []string{"example"}, Direction: Internal}, true},
		{"combined one miss", Filter{Types: []string{"acme.*"}, Direction: Outbound}, false},
	}
	for _, c := range cases {
		if got := c.f.Match(ev); got != c.want {
			t.Errorf("%s: Match = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestFilterMatchDeliveryMode(t *testing.T) {
	pushed := Event{Type: TypeTerminalDone, DeliveryMode: DeliveryModePush}
	pulled := Event{Type: "acme.ci_status"}

	cases := []struct {
		name string
		f    Filter
		ev   Event
		want bool
	}{
		{"empty delivery filter matches push", Filter{}, pushed, true},
		{"empty delivery filter matches pull (zero value)", Filter{}, pulled, true},
		{"delivery hit", Filter{DeliveryMode: DeliveryModePush}, pushed, true},
		{"delivery miss (pull requested, push event)", Filter{DeliveryMode: DeliveryModePull}, pushed, false},
		{"delivery miss (push requested, zero-value pull event)", Filter{DeliveryMode: DeliveryModePush}, pulled, false},
		{"pull filter matches an event with empty (unset) delivery_mode", Filter{DeliveryMode: DeliveryModePull}, pulled, true},
	}
	for _, c := range cases {
		if got := c.f.Match(c.ev); got != c.want {
			t.Errorf("%s: Match = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDeliveryModeNormalize(t *testing.T) {
	cases := []struct {
		name string
		m    DeliveryMode
		want DeliveryMode
	}{
		{"empty normalizes to pull", "", DeliveryModePull},
		{"pull stays pull", DeliveryModePull, DeliveryModePull},
		{"push stays push", DeliveryModePush, DeliveryModePush},
	}
	for _, c := range cases {
		if got := c.m.Normalize(); got != c.want {
			t.Errorf("%s: Normalize() = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestPlectEventNamespaceConstants(t *testing.T) {
	cases := map[string]string{
		"source":          SourcePlect,
		"tick source":     SourceTick,
		"instruction":     TypeInstruction,
		"channel error":   TypeChannelError,
		"status message":  TypeStatusMessage,
		"terminal prefix": TypeTerminalPrefix,
		"terminal done":   TypeTerminalDone,
		"terminal dead":   TypeTerminalDead,
		"tick review":     TypeTickReviewRequired,
		"tick escalated":  TypeTickEscalated,
		"judge recorded":  TypeJudgeRecorded,
		"chain attempt":   TypeChainAttempt,
	}
	for name, got := range cases {
		if got == "" {
			t.Fatalf("%s = %q, want plect namespace", name, got)
		}
	}
	if SourcePlect != "plect" {
		t.Fatalf("SourcePlect = %q, want plect", SourcePlect)
	}
	if TypeInstruction != "plect.instruction" {
		t.Fatalf("TypeInstruction = %q, want plect.instruction", TypeInstruction)
	}
	if TypeStatusMessage != "plect.status_message" {
		t.Fatalf("TypeStatusMessage = %q, want plect.status_message", TypeStatusMessage)
	}
}

func TestSplitCSV(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"blank", "   ", nil},
		{"single", "acme", []string{"acme"}},
		{"comma no space", "acme,widget", []string{"acme", "widget"}},
		{"comma with space", "acme, widget", []string{"acme", "widget"}},
		{"leading/trailing space", "  acme , widget  ", []string{"acme", "widget"}},
		{"empty element dropped", "acme,,widget", []string{"acme", "widget"}},
		{"all-blank elements", " , , ", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SplitCSV(c.in)
			if !slices.Equal(got, c.want) {
				t.Errorf("SplitCSV(%q) = %#v, want %#v", c.in, got, c.want)
			}
		})
	}
}
