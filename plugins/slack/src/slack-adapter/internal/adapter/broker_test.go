package adapter

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestBroker_SubscribeFindUnsubscribe(t *testing.T) {
	b := NewBroker("", nil)

	got := b.Subscribe(Subscriber{
		ThreadTS:   "1111.000",
		ChannelID:  "C123",
		SocketPath: "/run/x.sock",
	})
	if got.Since.IsZero() {
		t.Fatalf("Since should be auto-populated when zero")
	}

	found, ok := b.Find("1111.000")
	if !ok {
		t.Fatalf("expected to find subscriber")
	}
	if found.ChannelID != "C123" || found.SocketPath != "/run/x.sock" {
		t.Fatalf("unexpected subscriber: %+v", found)
	}

	if _, ok := b.Find("missing"); ok {
		t.Fatalf("expected miss for unknown thread_ts")
	}

	removed, ok := b.Unsubscribe("1111.000")
	if !ok {
		t.Fatalf("expected Unsubscribe to report removal")
	}
	if removed.ThreadTS != "1111.000" {
		t.Fatalf("removed wrong subscriber: %+v", removed)
	}
	if _, ok := b.Find("1111.000"); ok {
		t.Fatalf("Find should miss after Unsubscribe")
	}

	if _, ok := b.Unsubscribe("1111.000"); ok {
		t.Fatalf("second Unsubscribe should report no-op")
	}
}

func TestBroker_BySessionResolvesSubscriber(t *testing.T) {
	b := NewBroker("", nil)
	b.Subscribe(Subscriber{ThreadTS: "t1", ChannelID: "C", SocketPath: "/a", SessionName: "owner/repo-1"})
	b.Subscribe(Subscriber{ThreadTS: "t2", ChannelID: "C", SocketPath: "/b", SessionName: "owner/repo-2"})

	got, ok := b.BySession("owner/repo-1")
	if !ok {
		t.Fatalf("expected to find subscriber by session_name")
	}
	if got.ThreadTS != "t1" {
		t.Errorf("got ThreadTS %q, want t1", got.ThreadTS)
	}

	if _, ok := b.BySession("unknown"); ok {
		t.Errorf("expected miss for unknown session_name")
	}
}

// Migrated subscribers (persisted before session_name was added) load with
// SessionName == "". BySession("") must NOT match them — otherwise an
// unrelated /notify with an empty session_name would deliver to a random
// migrated entry.
func TestBroker_BySessionEmptyIsAlwaysMiss(t *testing.T) {
	b := NewBroker("", nil)
	b.Subscribe(Subscriber{ThreadTS: "t1", ChannelID: "C", SocketPath: "/a"})
	if _, ok := b.BySession(""); ok {
		t.Fatalf("BySession(\"\") must not match migrated entries")
	}
}

func TestBroker_SubscribePreservesExplicitSince(t *testing.T) {
	b := NewBroker("", nil)
	t0 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	got := b.Subscribe(Subscriber{ThreadTS: "x", Since: t0})
	if !got.Since.Equal(t0) {
		t.Fatalf("explicit Since should be preserved, got %v", got.Since)
	}
}

func TestBroker_SubscribeReplacesExisting(t *testing.T) {
	b := NewBroker("", nil)
	b.Subscribe(Subscriber{ThreadTS: "t", ChannelID: "C1", SocketPath: "/old"})
	b.Subscribe(Subscriber{ThreadTS: "t", ChannelID: "C2", SocketPath: "/new"})

	got, _ := b.Find("t")
	if got.ChannelID != "C2" || got.SocketPath != "/new" {
		t.Fatalf("Subscribe should replace, got %+v", got)
	}
}

// A socket_path shared by two different thread_ts registrations is the
// exact shape that lets a reply misroute to the channel: the second
// Subscribe for the same socket must evict the first rather than
// coexist with it.
func TestBroker_SubscribeReplacesExistingRegistrationForSameSocket(t *testing.T) {
	b := NewBroker("", nil)
	b.Subscribe(Subscriber{ThreadTS: "1111111111.100000", ChannelID: "C1", SocketPath: "/run/x.sock", SessionName: "owner/repo-1"})
	b.Subscribe(Subscriber{ThreadTS: "1111111111.200000", ChannelID: "C1", SocketPath: "/run/x.sock", SessionName: "owner/repo-1"})

	if _, ok := b.Find("1111111111.100000"); ok {
		t.Fatalf("stale registration for the same socket should have been replaced")
	}
	got, ok := b.Find("1111111111.200000")
	if !ok || got.SocketPath != "/run/x.sock" {
		t.Fatalf("expected the new registration to be present, got %+v (ok=%v)", got, ok)
	}
	if len(b.List()) != 1 {
		t.Fatalf("List length = %d, want 1 (one live registration per socket)", len(b.List()))
	}
}

// A Subscribe for one thread_ts must not disturb a different socket's
// registration under a different thread_ts.
func TestBroker_SubscribeLeavesOtherSocketsAlone(t *testing.T) {
	b := NewBroker("", nil)
	b.Subscribe(Subscriber{ThreadTS: "1111111111.100000", ChannelID: "C1", SocketPath: "/run/a.sock"})
	b.Subscribe(Subscriber{ThreadTS: "2222222222.200000", ChannelID: "C1", SocketPath: "/run/b.sock"})

	if _, ok := b.Find("1111111111.100000"); !ok {
		t.Fatalf("unrelated socket's registration should be unaffected")
	}
	if len(b.List()) != 2 {
		t.Fatalf("List length = %d, want 2", len(b.List()))
	}
}

func TestBroker_UnsubscribeBySessionRemovesEveryMatch(t *testing.T) {
	b := NewBroker("", nil)
	b.Subscribe(Subscriber{ThreadTS: "1111111111.100000+op.100000+ops_chat", ChannelID: "C", SocketPath: "/a", SessionName: "owner/repo-1"})
	b.Subscribe(Subscriber{ThreadTS: "1111111111.100000", ChannelID: "C", SocketPath: "/b", SessionName: "owner/repo-1"})
	b.Subscribe(Subscriber{ThreadTS: "2222222222.200000", ChannelID: "C", SocketPath: "/c", SessionName: "owner/repo-2"})

	removed := b.UnsubscribeBySession("owner/repo-1")
	if len(removed) != 2 {
		t.Fatalf("removed length = %d, want 2", len(removed))
	}
	if _, ok := b.BySession("owner/repo-1"); ok {
		t.Fatalf("every registration for the session should be gone")
	}
	if _, ok := b.BySession("owner/repo-2"); !ok {
		t.Fatalf("other sessions' registrations must be untouched")
	}
}

func TestBroker_UnsubscribeBySessionUnknownIsNoop(t *testing.T) {
	b := NewBroker("", nil)
	b.Subscribe(Subscriber{ThreadTS: "1111111111.100000", SessionName: "owner/repo-1"})

	if removed := b.UnsubscribeBySession("unknown"); len(removed) != 0 {
		t.Fatalf("expected no removals, got %+v", removed)
	}
	if removed := b.UnsubscribeBySession(""); len(removed) != 0 {
		t.Fatalf("empty session_name must match nothing, got %+v", removed)
	}
	if len(b.List()) != 1 {
		t.Fatalf("unrelated registration should be untouched")
	}
}

// A watermark on a session-wide removal must tombstone the same way a
// single Unsubscribe does, so a same-session resubscribe still restores it.
func TestBroker_UnsubscribeBySessionTombstonesWatermark(t *testing.T) {
	b := NewBroker("", nil)
	b.Subscribe(Subscriber{ThreadTS: "1111111111.100000", SessionName: "owner/repo-1", DeliveredThrough: "1111111111.100005"})

	b.UnsubscribeBySession("owner/repo-1")

	got := b.Subscribe(Subscriber{ThreadTS: "1111111111.100000", SessionName: "owner/repo-1"})
	if got.DeliveredThrough != "1111111111.100005" {
		t.Fatalf("DeliveredThrough = %q, want restored from tombstone", got.DeliveredThrough)
	}
}

func TestBroker_SubscribePreservesDeliveryWatermarkWhenReplacing(t *testing.T) {
	b := NewBroker("", nil)
	b.Subscribe(Subscriber{ThreadTS: "t", ChannelID: "C1", SocketPath: "/old", DeliveredThrough: "1000.000005"})
	b.Subscribe(Subscriber{ThreadTS: "t", ChannelID: "C2", SocketPath: "/new"})

	got, _ := b.Find("t")
	if got.DeliveredThrough != "1000.000005" {
		t.Fatalf("DeliveredThrough = %q, want preserved watermark", got.DeliveredThrough)
	}
}

func TestBroker_MarkDeliveredAdvancesWatermark(t *testing.T) {
	b := NewBroker("", nil)
	b.Subscribe(Subscriber{ThreadTS: "t", DeliveredThrough: "1000.000005"})

	got, ok := b.MarkDelivered("t", "1000.000008")
	if !ok {
		t.Fatalf("MarkDelivered should find the subscriber")
	}
	if got.DeliveredThrough != "1000.000008" {
		t.Fatalf("DeliveredThrough = %q, want advanced watermark", got.DeliveredThrough)
	}

	got, ok = b.MarkDelivered("t", "1000.000006")
	if !ok {
		t.Fatalf("MarkDelivered should find the subscriber")
	}
	if got.DeliveredThrough != "1000.000008" {
		t.Fatalf("DeliveredThrough = %q, want unchanged watermark", got.DeliveredThrough)
	}
}

func TestBroker_UnsubscribeThenSubscribeSameSessionRestoresWatermark(t *testing.T) {
	b := NewBroker("", nil)
	b.Subscribe(Subscriber{ThreadTS: "t", SessionName: "owner/repo-1", DeliveredThrough: "1000.000005"})

	removed, ok := b.Unsubscribe("t")
	if !ok {
		t.Fatalf("expected Unsubscribe to report removal")
	}
	if removed.DeliveredThrough != "1000.000005" {
		t.Fatalf("removed watermark = %q, want 1000.000005", removed.DeliveredThrough)
	}
	if _, ok := b.Find("t"); ok {
		t.Fatalf("Find should miss right after Unsubscribe")
	}

	got := b.Subscribe(Subscriber{ThreadTS: "t", SessionName: "owner/repo-1"})
	if got.DeliveredThrough != "1000.000005" {
		t.Fatalf("DeliveredThrough = %q, want restored from tombstone", got.DeliveredThrough)
	}
}

func TestBroker_UnsubscribeThenSubscribeDifferentSessionGetsNoWatermark(t *testing.T) {
	b := NewBroker("", nil)
	b.Subscribe(Subscriber{ThreadTS: "t", SessionName: "owner/repo-1", DeliveredThrough: "1000.000005"})
	b.Unsubscribe("t")

	got := b.Subscribe(Subscriber{ThreadTS: "t", SessionName: "owner/repo-2"})
	if got.DeliveredThrough != "" {
		t.Fatalf("DeliveredThrough = %q, want empty for a different session", got.DeliveredThrough)
	}
}

func TestBroker_UnsubscribeWithoutWatermarkLeavesNoTombstone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subscribers.json")
	b := NewBroker(path, discardLogger())

	b.Subscribe(Subscriber{ThreadTS: "t", SessionName: "owner/repo-1"})
	b.Unsubscribe("t")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ps persistedState
	if err := json.Unmarshal(data, &ps); err != nil {
		t.Fatal(err)
	}
	if len(ps.Tombstones) != 0 {
		t.Fatalf("expected no tombstone for a subscriber with no watermark, got %+v", ps.Tombstones)
	}
}

func TestBroker_SubscribeConsumesTombstone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subscribers.json")
	b := NewBroker(path, discardLogger())

	b.Subscribe(Subscriber{ThreadTS: "t", SessionName: "owner/repo-1", DeliveredThrough: "1000.000005"})
	b.Unsubscribe("t")
	b.Subscribe(Subscriber{ThreadTS: "t", SessionName: "owner/repo-1"})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ps persistedState
	if err := json.Unmarshal(data, &ps); err != nil {
		t.Fatal(err)
	}
	if len(ps.Tombstones) != 0 {
		t.Fatalf("expected tombstone consumed after resubscribe, got %+v", ps.Tombstones)
	}
}

func TestBroker_TombstoneSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subscribers.json")

	func() {
		b := NewBroker(path, discardLogger())
		b.Subscribe(Subscriber{ThreadTS: "t", SessionName: "owner/repo-1", DeliveredThrough: "1000.000005"})
		b.Unsubscribe("t")
	}()

	// A fresh Broker over the same path models an adapter process restart.
	b := NewBroker(path, discardLogger())
	got := b.Subscribe(Subscriber{ThreadTS: "t", SessionName: "owner/repo-1"})
	if got.DeliveredThrough != "1000.000005" {
		t.Fatalf("DeliveredThrough = %q, want restored from tombstone after restart", got.DeliveredThrough)
	}
}

func TestBroker_LoadPrunesTombstonesOlderThan30Days(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subscribers.json")

	old := time.Now().Add(-tombstoneTTL - time.Hour)
	initial := persistedState{
		Tombstones: []Tombstone{
			{ThreadTS: "t", SessionName: "owner/repo-1", DeliveredThrough: "1000.000005", UnsubscribedAt: old},
		},
	}
	data, _ := json.Marshal(initial)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	b := NewBroker(path, discardLogger())
	got := b.Subscribe(Subscriber{ThreadTS: "t", SessionName: "owner/repo-1"})
	if got.DeliveredThrough != "" {
		t.Fatalf("DeliveredThrough = %q, want empty (tombstone older than 30 days must not survive load)", got.DeliveredThrough)
	}
}

func TestBroker_PersistPrunesTombstonesOlderThan30Days(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subscribers.json")
	b := NewBroker(path, discardLogger())

	old := time.Now().Add(-tombstoneTTL - time.Hour)
	b.mu.Lock()
	b.tombstones[tombstoneKey("old-thread", "owner/repo-1")] = Tombstone{
		ThreadTS:         "old-thread",
		SessionName:      "owner/repo-1",
		DeliveredThrough: "1000.000005",
		UnsubscribedAt:   old,
	}
	b.mu.Unlock()

	// Any persist prunes, not just one touching the expired tombstone's own
	// thread_ts.
	b.Subscribe(Subscriber{ThreadTS: "other", SessionName: "owner/repo-2"})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ps persistedState
	if err := json.Unmarshal(data, &ps); err != nil {
		t.Fatal(err)
	}
	for _, tomb := range ps.Tombstones {
		if tomb.ThreadTS == "old-thread" {
			t.Fatalf("expected tombstone older than 30 days pruned on persist, got %+v", ps.Tombstones)
		}
	}
}

// Deliberate: no compatibility shim for the pre-tombstone bare-array shape,
// per the repo's no-backward-compat-code policy; see docs/migrations for
// the one-time conversion this requires instead.
func TestBroker_LoadRejectsLegacyBareArrayFileAsCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subscribers.json")

	legacy := []Subscriber{
		{ThreadTS: "1111.000", ChannelID: "C123", SocketPath: "/a", SessionName: "owner/repo-1", DeliveredThrough: "1000.000005", Since: time.Now()},
	}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	b := NewBroker(path, discardLogger())
	if got := b.List(); len(got) != 0 {
		t.Fatalf("expected empty broker for a legacy-format file, got %d entries", len(got))
	}
}

func TestBroker_List(t *testing.T) {
	b := NewBroker("", nil)
	if got := b.List(); len(got) != 0 {
		t.Fatalf("empty broker should return empty list, got %d", len(got))
	}

	b.Subscribe(Subscriber{ThreadTS: "a"})
	b.Subscribe(Subscriber{ThreadTS: "b"})
	got := b.List()
	if len(got) != 2 {
		t.Fatalf("List length = %d, want 2", len(got))
	}
	seen := map[string]bool{}
	for _, s := range got {
		seen[s.ThreadTS] = true
	}
	if !seen["a"] || !seen["b"] {
		t.Fatalf("List missing entries: %+v", got)
	}
}

func TestBroker_PersistOnSubscribeAndUnsubscribe(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subscribers.json")
	b := NewBroker(path, discardLogger())

	b.Subscribe(Subscriber{ThreadTS: "1111.000", ChannelID: "C123", SocketPath: "/x"})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected state file after subscribe: %v", err)
	}
	var got persistedState
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Subscribers) != 1 || got.Subscribers[0].ThreadTS != "1111.000" || got.Subscribers[0].ChannelID != "C123" {
		t.Fatalf("unexpected persisted state: %+v", got)
	}

	// No DeliveredThrough was ever set, so unsubscribe leaves no tombstone.
	b.Unsubscribe("1111.000")
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after unsubscribe: %v", err)
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode after unsubscribe: %v", err)
	}
	if len(got.Subscribers) != 0 {
		t.Fatalf("expected empty subscriber list after unsubscribe, got %+v", got)
	}
	if len(got.Tombstones) != 0 {
		t.Fatalf("expected no tombstone for a subscriber with no watermark, got %+v", got.Tombstones)
	}
}

// Two persisted registrations share one socket_path (a stale entry from
// before Subscribe enforced one-per-socket). Load must keep only the newer
// one, so a restart no longer pre-connects the socket to two thread
// destinations.
func TestBroker_LoadDedupesBySocketPathKeepingNewestSince(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subscribers.json")

	older := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)
	initial := persistedState{
		Subscribers: []Subscriber{
			{ThreadTS: "1111111111.100000+op.100000+ops_chat", ChannelID: "C", SocketPath: "/run/x.sock", SessionName: "s", Since: older},
			{ThreadTS: "1111111111.100000", ChannelID: "C", SocketPath: "/run/x.sock", SessionName: "s", Since: newer},
		},
	}
	data, _ := json.Marshal(initial)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	b := NewBroker(path, discardLogger())
	got := b.List()
	if len(got) != 1 {
		t.Fatalf("List length = %d, want 1 after dedup-on-load, got %+v", len(got), got)
	}
	if got[0].ThreadTS != "1111111111.100000" {
		t.Fatalf("kept entry = %+v, want the one with the newer Since", got[0])
	}

	// The dropped duplicate must not resurface if the process restarts again.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ps persistedState
	if err := json.Unmarshal(data, &ps); err != nil {
		t.Fatal(err)
	}
	if len(ps.Subscribers) != 1 {
		t.Fatalf("expected dedup to be persisted back to disk, got %+v", ps.Subscribers)
	}
}

// Registrations on different sockets must never be conflated by load-time
// dedup, even when several share the same socket in different groups.
func TestBroker_LoadDedupeOnlyAffectsSharedSockets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subscribers.json")

	t0 := time.Now()
	initial := persistedState{
		Subscribers: []Subscriber{
			{ThreadTS: "1111111111.100000", ChannelID: "C", SocketPath: "/run/a.sock", Since: t0},
			{ThreadTS: "2222222222.200000", ChannelID: "C", SocketPath: "/run/b.sock", Since: t0},
		},
	}
	data, _ := json.Marshal(initial)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	b := NewBroker(path, discardLogger())
	if got := b.List(); len(got) != 2 {
		t.Fatalf("List length = %d, want 2 (distinct sockets untouched by dedup)", len(got))
	}
}

func TestBroker_LoadRestoresState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subscribers.json")

	initial := persistedState{
		Subscribers: []Subscriber{
			{ThreadTS: "1111.000", ChannelID: "C123", SocketPath: "/a", Since: time.Now()},
			{ThreadTS: "2222.000", ChannelID: "C456", SocketPath: "/b", Since: time.Now()},
		},
	}
	data, _ := json.Marshal(initial)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	b := NewBroker(path, discardLogger())
	if len(b.List()) != 2 {
		t.Fatalf("expected 2 restored subs, got %d", len(b.List()))
	}
	if _, ok := b.Find("1111.000"); !ok {
		t.Fatalf("expected to find restored subscriber")
	}
}

func TestBroker_LoadMissingFileStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subscribers.json")
	b := NewBroker(path, discardLogger())
	if got := b.List(); len(got) != 0 {
		t.Fatalf("expected empty broker, got %d entries", len(got))
	}
}

func TestBroker_LoadCorruptFileStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subscribers.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := NewBroker(path, discardLogger())
	if got := b.List(); len(got) != 0 {
		t.Fatalf("expected empty broker after corrupt load, got %d", len(got))
	}

	// After the next Subscribe, persistence should overwrite the corrupt file.
	b.Subscribe(Subscriber{ThreadTS: "1111.000", ChannelID: "C", SocketPath: "/x"})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got persistedState
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode after recovery write: %v", err)
	}
	if len(got.Subscribers) != 1 {
		t.Fatalf("expected 1 entry after recovery write, got %d", len(got.Subscribers))
	}
}

func TestBroker_PersistRoundTripPreservesFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subscribers.json")

	t0 := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	{
		b := NewBroker(path, discardLogger())
		b.Subscribe(Subscriber{ThreadTS: "t1", ChannelID: "C1", SocketPath: "/s1", Since: t0})
	}

	b := NewBroker(path, discardLogger())
	got, ok := b.Find("t1")
	if !ok {
		t.Fatalf("expected to find subscriber after reload")
	}
	if got.ChannelID != "C1" || got.SocketPath != "/s1" {
		t.Fatalf("unexpected reloaded subscriber: %+v", got)
	}
	if !got.Since.Equal(t0) {
		t.Fatalf("Since not preserved: got %v want %v", got.Since, t0)
	}
}

func TestBroker_NoPersistenceWhenPathEmpty(t *testing.T) {
	dir := t.TempDir()
	// Ensure the directory has no state file even if we accidentally persist.
	b := NewBroker("", discardLogger())
	b.Subscribe(Subscriber{ThreadTS: "t", ChannelID: "C", SocketPath: "/x"})
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("expected no files written when path is empty, got %v", entries)
	}
}

// Concurrent Subscribe / Unsubscribe must never leave subscribers.json
// partially-written or unparseable. The tmp→rename atomic-write contract
// means any reader sees either the previous or the new full snapshot.
func TestBroker_ConcurrentPersistKeepsFileValid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subscribers.json")
	b := NewBroker(path, discardLogger())

	const goroutines = 8
	const opsPerGoroutine = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := range goroutines {
		go func(id int) {
			defer wg.Done()
			for i := range opsPerGoroutine {
				ts := fmt.Sprintf("g%d-%d", id, i)
				b.Subscribe(Subscriber{ThreadTS: ts, ChannelID: "C", SocketPath: "/x"})
				b.Unsubscribe(ts)
			}
		}(g)
	}

	// Concurrently re-read the persisted file while writes are happening.
	// Every snapshot we observe must be valid JSON of the documented shape.
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			select {
			case <-readDone:
				return
			default:
			}
			data, err := os.ReadFile(path)
			if errors.Is(err, fs.ErrNotExist) || len(data) == 0 {
				continue
			}
			if err != nil {
				t.Errorf("read: %v", err)
				return
			}
			var ps persistedState
			if err := json.Unmarshal(data, &ps); err != nil {
				t.Errorf("subscribers.json corrupt mid-flight: %v (%q)", err, data)
				return
			}
		}
	}()
	wg.Wait()
	// Stop the reader after writes complete.
	go func() { readDone <- struct{}{} }()
	<-readDone

	// Final state should be valid JSON for an empty subscriber list.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("final read: %v", err)
	}
	var ps persistedState
	if err := json.Unmarshal(data, &ps); err != nil {
		t.Fatalf("final subscribers.json corrupt: %v (%q)", err, data)
	}
	if len(ps.Subscribers) != 0 {
		t.Fatalf("final subs length = %d, want 0", len(ps.Subscribers))
	}
}

// After concurrent Subscribe/Unsubscribe, the on-disk snapshot must match
// the in-memory map exactly. A reorder-after-unlock bug would let an older
// snapshot land last and leave the file holding entries that were removed
// in memory (or vice versa), so reload after restart would resurrect dead
// subscriptions. This is stronger than just "file parses as valid JSON".
func TestBroker_ConcurrentPersistMatchesMemory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subscribers.json")
	b := NewBroker(path, discardLogger())

	const goroutines = 16
	const opsPerGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := range goroutines {
		go func(id int) {
			defer wg.Done()
			for i := range opsPerGoroutine {
				ts := fmt.Sprintf("g%d-%d", id, i)
				b.Subscribe(Subscriber{ThreadTS: ts, ChannelID: "C", SocketPath: "/x"})
				if i%2 == 1 {
					// Half of the entries are removed; the other half stays.
					b.Unsubscribe(ts)
				}
			}
		}(g)
	}
	wg.Wait()

	memSet := map[string]bool{}
	for _, s := range b.List() {
		memSet[s.ThreadTS] = true
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ps persistedState
	if err := json.Unmarshal(data, &ps); err != nil {
		t.Fatalf("disk corrupt: %v", err)
	}
	diskSet := map[string]bool{}
	for _, s := range ps.Subscribers {
		diskSet[s.ThreadTS] = true
	}

	if len(memSet) != len(diskSet) {
		t.Fatalf("size mismatch after concurrent ops: mem=%d disk=%d", len(memSet), len(diskSet))
	}
	for k := range memSet {
		if !diskSet[k] {
			t.Fatalf("entry %q in memory but not on disk (stale persist won)", k)
		}
	}
	for k := range diskSet {
		if !memSet[k] {
			t.Fatalf("entry %q on disk but not in memory (resurrected on next load)", k)
		}
	}
}

// Lock down the on-disk wire format so downstream readers don't silently
// break if the JSON shape drifts. Top-level is an object keyed
// "subscribers" (and "tombstones" when non-empty) — not a bare array — so a
// tombstones list can live in the same atomic snapshot as the subscribers.
func TestBroker_PersistedJSONWireFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subscribers.json")
	b := NewBroker(path, discardLogger())

	t0 := time.Date(2026, 5, 17, 12, 34, 56, 0, time.UTC)
	b.Subscribe(Subscriber{
		ThreadTS:   "1111.000",
		ChannelID:  "C123",
		SocketPath: "/run/x.sock",
		Since:      t0,
	})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var asObj map[string]any
	if err := json.Unmarshal(data, &asObj); err != nil {
		t.Fatalf("expected top-level to be a JSON object: %v (%q)", err, data)
	}
	subsField, ok := asObj["subscribers"].([]any)
	if !ok {
		t.Fatalf(`expected top-level "subscribers" array, got %+v`, asObj)
	}
	if len(subsField) != 1 {
		t.Fatalf("subscribers length = %d, want 1", len(subsField))
	}
	if _, ok := asObj["tombstones"]; ok {
		t.Errorf(`no tombstone was created, expected "tombstones" key omitted, got %+v`, asObj)
	}

	got, ok := subsField[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected subscriber shape: %+v", subsField[0])
	}
	wantKeys := []string{"thread_ts", "channel_id", "socket_path", "since"}
	for _, k := range wantKeys {
		if _, ok := got[k]; !ok {
			t.Errorf("missing key %q in serialized subscriber: %+v", k, got)
		}
	}
	if got["thread_ts"] != "1111.000" {
		t.Errorf("thread_ts = %v, want 1111.000", got["thread_ts"])
	}
	if got["channel_id"] != "C123" {
		t.Errorf("channel_id = %v, want C123", got["channel_id"])
	}
	if got["socket_path"] != "/run/x.sock" {
		t.Errorf("socket_path = %v, want /run/x.sock", got["socket_path"])
	}
}
