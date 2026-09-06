package mcpserver

import (
	"context"
	"testing"

	"github.com/kecbigmt/plecture/contracts/event"
)

func TestHandleEventShow_ReturnsPublishedEvent(t *testing.T) {
	setUpConfigHome(t)

	pub, err := handleEventPublish(context.Background(), reqWith(map[string]any{
		"session": "owner/repo-1",
		"type":    event.TypeUserNote,
		"summary": "hello",
	}))
	if err != nil {
		t.Fatalf("handleEventPublish: %v", err)
	}
	published := decodeJSONResult(t, pub)
	ev, ok := published["event"].(map[string]any)
	if !ok {
		t.Fatalf("event field missing or wrong type: %#v", published["event"])
	}
	id, _ := ev["id"].(string)
	if id == "" {
		t.Fatal("published event has no id")
	}

	result, err := handleEventShow(context.Background(), reqWith(map[string]any{
		"session":  "owner/repo-1",
		"event_id": id,
	}))
	if err != nil {
		t.Fatalf("handleEventShow: %v", err)
	}
	out := decodeJSONResult(t, result)
	shown, ok := out["event"].(map[string]any)
	if !ok {
		t.Fatalf("event field missing or wrong type: %#v", out["event"])
	}
	if shown["id"] != id {
		t.Errorf("id = %v, want %v", shown["id"], id)
	}
	if shown["summary"] != "hello" {
		t.Errorf("summary = %v, want hello", shown["summary"])
	}
}

// A caller still naming the retired delivery_mode input must be told loudly
// that it does nothing, rather than have the argument silently ignored and
// the caller believe its delivery preference was honored.
func TestHandleEventPublish_RejectsDeliveryModeArgument(t *testing.T) {
	setUpConfigHome(t)

	result, err := handleEventPublish(context.Background(), reqWith(map[string]any{
		"session":       "owner/repo-1",
		"type":          event.TypeUserNote,
		"delivery_mode": "push",
	}))
	if err != nil {
		t.Fatalf("handleEventPublish: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result for a delivery_mode argument")
	}
}

func TestHandleEventShow_UnknownIDReturnsError(t *testing.T) {
	setUpConfigHome(t)

	if _, err := handleEventPublish(context.Background(), reqWith(map[string]any{
		"session": "owner/repo-1",
		"type":    event.TypeUserNote,
	})); err != nil {
		t.Fatalf("handleEventPublish: %v", err)
	}

	result, err := handleEventShow(context.Background(), reqWith(map[string]any{
		"session":  "owner/repo-1",
		"event_id": "does-not-exist",
	}))
	if err != nil {
		t.Fatalf("handleEventShow: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result for unknown event id")
	}
}

// plect_event_list's types/source params must split on comma and trim
// whitespace exactly like the CLI's --type/--source flags (event.SplitCSV is
// the shared implementation for both).
func TestHandleEventList_FiltersTypesAndSourceWithCommaAndWhitespace(t *testing.T) {
	setUpConfigHome(t)

	publish := func(typ, source string) {
		t.Helper()
		if _, err := handleEventPublish(context.Background(), reqWith(map[string]any{
			"session": "owner/repo-1",
			"type":    typ,
			"source":  source,
		})); err != nil {
			t.Fatalf("handleEventPublish(%q, %q): %v", typ, source, err)
		}
	}
	publish(event.TypeUserNote, event.SourceCLI)
	publish(event.TypeUserEmit, "chat")
	publish("widget.reply", "gadget")

	result, err := handleEventList(context.Background(), reqWith(map[string]any{
		"session": "owner/repo-1",
		"types":   event.TypeUserNote + " , widget.reply",
		"source":  " " + event.SourceCLI + ",  gadget ",
	}))
	if err != nil {
		t.Fatalf("handleEventList: %v", err)
	}
	out := decodeJSONResult(t, result)
	events, ok := out["events"].([]any)
	if !ok {
		t.Fatalf("events field missing or wrong type: %#v", out["events"])
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2: %#v", len(events), events)
	}
	for _, e := range events {
		ev := e.(map[string]any)
		typ := ev["type"].(string)
		if typ != event.TypeUserNote && typ != "widget.reply" {
			t.Errorf("unexpected event type in filtered result: %q", typ)
		}
	}
}

func TestHandleEventShow_RequiresArgs(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
	}{
		{"missing session", map[string]any{"event_id": "e1"}},
		{"missing event_id", map[string]any{"session": "owner/repo-1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := handleEventShow(context.Background(), reqWith(tc.args))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !result.IsError {
				t.Fatal("expected error result for missing required argument")
			}
		})
	}
}
