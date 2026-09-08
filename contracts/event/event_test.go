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

func TestPlectEventNamespaceConstants(t *testing.T) {
	cases := map[string]string{
		"source":                      SourcePlect,
		"tick source":                 SourceTick,
		"instruction":                 TypeInstruction,
		"channel error":               TypeChannelError,
		"status message":              TypeStatusMessage,
		"terminal prefix":             TypeTerminalPrefix,
		"terminal done":               TypeTerminalDone,
		"terminal dead":               TypeTerminalDead,
		"tick review":                 TypeTickReviewRequired,
		"tick escalated":              TypeTickEscalated,
		"judge recorded":              TypeJudgeRecorded,
		"chain attempt":               TypeChainAttempt,
		"message":                     TypeMessage,
		"message chunk":               TypeMessageChunk,
		"meta message id":             MetaMessageID,
		"meta message id origin":      MetaMessageIDOrigin,
		"meta role":                   MetaRole,
		"meta source":                 MetaSource,
		"meta kind":                   MetaKind,
		"meta index":                  MetaIndex,
		"meta final":                  MetaFinal,
		"meta turn id":                MetaTurnID,
		"meta turn index":             MetaTurnIndex,
		"meta step index":             MetaStepIndex,
		"meta run id":                 MetaRunID,
		"meta source seq":             MetaSourceSeq,
		"meta raw":                    MetaRaw,
		"meta interim":                MetaInterim,
		"meta stop reason":            MetaStopReason,
		"meta truncated":              MetaTruncated,
		"meta model":                  MetaModel,
		"meta provider":               MetaProvider,
		"meta surface":                MetaSurface,
		"meta agent id":               MetaAgentID,
		"meta parent agent id":        MetaParentAgentID,
		"meta depth":                  MetaDepth,
		"meta agent role":             MetaAgentRole,
		"meta ordering":               MetaOrdering,
		"meta block index":            MetaBlockIndex,
		"role assistant":              RoleAssistant,
		"message id origin native":    MessageIDOriginNative,
		"message id origin synthetic": MessageIDOriginSynthetic,
		"chunk kind text":             ChunkKindText,
		"chunk kind reasoning":        ChunkKindReasoning,
		"stop reason completed":       StopReasonCompleted,
		"stop reason max tokens":      StopReasonMaxTokens,
		"stop reason aborted":         StopReasonAborted,
		"stop reason error":           StopReasonError,
		"stop reason interrupted":     StopReasonInterrupted,
		"ordering strict":             OrderingStrict,
		"ordering best effort":        OrderingBestEffort,
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
	if TypeMessage != "plect.message" {
		t.Fatalf("TypeMessage = %q, want plect.message", TypeMessage)
	}
	if TypeMessageChunk != "plect.message_chunk" {
		t.Fatalf("TypeMessageChunk = %q, want plect.message_chunk", TypeMessageChunk)
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
