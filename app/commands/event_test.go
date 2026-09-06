package commands

import (
	"slices"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/contracts/event"
)

// deliveryLabel replaces the retired stored DeliveryMode field: the DELIVERY
// column derives from whether the type carries the terminal-event prefix,
// not from a value a producer could set independently of what it published.
func TestDeliveryLabel_DerivesFromTerminalTypePrefix(t *testing.T) {
	cases := []struct {
		name string
		typ  string
		want string
	}{
		{"terminal done", event.TypeTerminalDone, "push"},
		{"terminal escalate", event.TypeTerminalEscalate, "push"},
		{"terminal dead", event.TypeTerminalDead, "push"},
		{"ordinary progress event", event.TypeUserNote, "pull"},
		{"a type merely containing the terminal word is not the prefix", "plect.terminal", "pull"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := deliveryLabel(event.Event{Type: c.typ})
			if got != c.want {
				t.Errorf("deliveryLabel(Type: %q) = %q, want %q", c.typ, got, c.want)
			}
		})
	}
}

func TestSplitTypesArg(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, nil},
		{"single repeatable", []string{"github.*"}, []string{"github.*"}},
		{"repeatable, no comma", []string{"github.*", "claude.reply"}, []string{"github.*", "claude.reply"}},
		{"single comma-separated", []string{"github.*,claude.reply"}, []string{"github.*", "claude.reply"}},
		{"comma-separated with whitespace", []string{"github.*, claude.reply "}, []string{"github.*", "claude.reply"}},
		{"repeatable each comma-separated", []string{"github.*,claude.reply", "slack.message"}, []string{"github.*", "claude.reply", "slack.message"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := splitTypesArg(c.in)
			if !slices.Equal(got, c.want) {
				t.Errorf("splitTypesArg(%#v) = %#v, want %#v", c.in, got, c.want)
			}
		})
	}
}

// buildEventFilter reads package-level flag vars set by cobra; this exercises
// it the same way the CLI/MCP handlers must agree: comma-separated and
// repeatable --type inputs both split and trim, matching MCP's types/source.
func TestBuildEventFilter_SplitsAndTrimsTypesAndSources(t *testing.T) {
	origTypes, origSource := eventTypes, eventSource
	origDirection, origLimit := eventDirection, eventLimit
	t.Cleanup(func() {
		eventTypes, eventSource = origTypes, origSource
		eventDirection, eventLimit = origDirection, origLimit
	})

	eventTypes = []string{"github.*, claude.reply", "slack.message"}
	eventSource = " github, slack "
	eventDirection = "inbound"
	eventLimit = 5

	f := buildEventFilter()

	wantTypes := []string{"github.*", "claude.reply", "slack.message"}
	if !slices.Equal(f.Types, wantTypes) {
		t.Errorf("Types = %#v, want %#v", f.Types, wantTypes)
	}
	wantSources := []string{"github", "slack"}
	if !slices.Equal(f.Sources, wantSources) {
		t.Errorf("Sources = %#v, want %#v", f.Sources, wantSources)
	}
}

func TestParseMeta_NoPairsReturnsNil(t *testing.T) {
	got, err := parseMeta(nil)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got != nil {
		t.Errorf("meta = %#v, want nil", got)
	}
}

func TestParseMeta_SplitsKeyValuePairs(t *testing.T) {
	got, err := parseMeta([]string{"thread_ts=1234567890.123456", "channel_id=C01ABCDEF"})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	want := map[string]string{"thread_ts": "1234567890.123456", "channel_id": "C01ABCDEF"}
	if len(got) != len(want) || got["thread_ts"] != want["thread_ts"] || got["channel_id"] != want["channel_id"] {
		t.Errorf("meta = %#v, want %#v", got, want)
	}
}

// A value itself containing "=" (e.g. a base64 or URL-encoded value) must
// survive intact — parseMeta splits on the first "=" only.
func TestParseMeta_ValueContainingEqualsSurvivesIntact(t *testing.T) {
	got, err := parseMeta([]string{"token=abc=def=="})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got["token"] != "abc=def==" {
		t.Errorf("meta[token] = %q, want %q", got["token"], "abc=def==")
	}
}

func TestParseMeta_MissingEqualsIsAnError(t *testing.T) {
	_, err := parseMeta([]string{"no-equals-sign"})
	if err == nil {
		t.Fatal("err = nil, want an error naming the malformed pair")
	}
	if !strings.Contains(err.Error(), "no-equals-sign") {
		t.Errorf("err = %v, want it to name the offending pair", err)
	}
}

// The session argument and --subtree name mutually exclusive scopes: a
// cross-session view takes no session argument, and a single-session view
// requires exactly one.
func TestValidateScopeArgs(t *testing.T) {
	origSubtree := eventSubtree
	t.Cleanup(func() { eventSubtree = origSubtree })

	t.Run("subtree with no args is valid", func(t *testing.T) {
		eventSubtree = "owner/repo-1"
		if err := validateScopeArgs(nil); err != nil {
			t.Errorf("err = %v, want nil", err)
		}
	})

	t.Run("subtree with a session arg is an error", func(t *testing.T) {
		eventSubtree = "owner/repo-1"
		err := validateScopeArgs([]string{"owner/repo-2"})
		if err == nil || !strings.Contains(err.Error(), "owner/repo-2") {
			t.Errorf("err = %v, want an error naming the extra session argument", err)
		}
	})

	t.Run("no subtree and no session arg is an error", func(t *testing.T) {
		eventSubtree = ""
		if err := validateScopeArgs(nil); err == nil {
			t.Error("err = nil, want an error requiring a session or --subtree")
		}
	})

	t.Run("no subtree and one session arg is valid", func(t *testing.T) {
		eventSubtree = ""
		if err := validateScopeArgs([]string{"owner/repo-1"}); err != nil {
			t.Errorf("err = %v, want nil", err)
		}
	})

	t.Run("no subtree and two session args is an error", func(t *testing.T) {
		eventSubtree = ""
		if err := validateScopeArgs([]string{"a", "b"}); err == nil {
			t.Error("err = nil, want an error")
		}
	})
}
