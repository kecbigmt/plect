package legacyimport

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func writeSessionDir(t *testing.T, eventsRoot, name string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(eventsRoot, encodeSessionDir(name))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	for filename, content := range files {
		if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", filename, err)
		}
	}
	return dir
}

func TestReadSessionDir_DecodesLogGenAndCursors(t *testing.T) {
	root := t.TempDir()
	writeSessionDir(t, root, "case1", map[string]string{
		"log.jsonl": `{"id":"01A","session_name":"case1","time":"2026-01-01T00:00:00Z","type":"user.note","source":"cli","direction":"inbound","summary":"first"}` + "\n" +
			`{"id":"01B","session_name":"case1","time":"2026-01-01T00:00:01Z","type":"user.note","source":"cli","direction":"outbound","summary":"second"}` + "\n",
		".gen":               "01GENID\n",
		".cursor.dispatcher": "",
	})
	// The dispatcher cursor value must line up with a real line boundary;
	// compute it from the actual log content rather than hardcoding a guess.
	logPath := filepath.Join(root, encodeSessionDir("case1"), "log.jsonl")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	firstLineEnd := strings.IndexByte(string(data), '\n') + 1
	if err := os.WriteFile(filepath.Join(root, encodeSessionDir("case1"), ".cursor.dispatcher"), []byte(strconv.Itoa(firstLineEnd)), 0o644); err != nil {
		t.Fatal(err)
	}

	sl, err := ReadSessionDir(root, "case1")
	if err != nil {
		t.Fatalf("ReadSessionDir: %v", err)
	}
	if sl.GenID != "01GENID" {
		t.Errorf("GenID = %q, want 01GENID", sl.GenID)
	}
	if len(sl.Events) != 2 {
		t.Fatalf("Events = %d, want 2", len(sl.Events))
	}
	if sl.Events[0].ID != "01A" || sl.Events[1].ID != "01B" {
		t.Errorf("Events = %+v, want 01A then 01B in order", sl.Events)
	}
	if len(sl.UnknownFiles) != 0 {
		t.Errorf("UnknownFiles = %v, want none", sl.UnknownFiles)
	}
	offset, ok := sl.CursorOffsets["delivery"]
	if !ok {
		t.Fatal("CursorOffsets[delivery] missing (dispatcher must map to delivery)")
	}
	seq, ok := sl.Translate(offset)
	if !ok || seq != 2 {
		t.Fatalf("Translate(%d) = (%d, %v), want (2, true) (past line 1, so next is event 2)", offset, seq, ok)
	}
}

func TestReadSessionDir_NoDirectionDefaultsToInternal(t *testing.T) {
	root := t.TempDir()
	writeSessionDir(t, root, "case1", map[string]string{
		"log.jsonl": `{"id":"01A","session_name":"case1","time":"2026-01-01T00:00:00Z","type":"x","source":"cli","summary":"no direction"}` + "\n",
	})
	sl, err := ReadSessionDir(root, "case1")
	if err != nil {
		t.Fatalf("ReadSessionDir: %v", err)
	}
	if sl.InternalBackfilled != 1 {
		t.Errorf("InternalBackfilled = %d, want 1", sl.InternalBackfilled)
	}
	if sl.Events[0].Direction != "internal" {
		t.Errorf("Direction = %q, want internal", sl.Events[0].Direction)
	}
}

func TestReadSessionDir_DiscardsTrailingPartialLine(t *testing.T) {
	root := t.TempDir()
	writeSessionDir(t, root, "case1", map[string]string{
		"log.jsonl": `{"id":"01A","session_name":"case1","time":"2026-01-01T00:00:00Z","type":"x","source":"cli","direction":"internal","summary":"complete"}` + "\n" +
			`{"id":"01B","session_name":"case1"`, // torn, no trailing newline
	})
	sl, err := ReadSessionDir(root, "case1")
	if err != nil {
		t.Fatalf("ReadSessionDir: %v", err)
	}
	if len(sl.Events) != 1 || sl.Events[0].ID != "01A" {
		t.Fatalf("Events = %+v, want just the one complete record", sl.Events)
	}
}

func TestReadSessionDir_RejectsMalformedCompleteLine(t *testing.T) {
	root := t.TempDir()
	writeSessionDir(t, root, "case1", map[string]string{
		"log.jsonl": "{not valid json}\n",
	})
	if _, err := ReadSessionDir(root, "case1"); err == nil {
		t.Fatal("ReadSessionDir over a malformed complete line must fail, not skip it")
	}
}

func TestReadSessionDir_RejectsDuplicateEventID(t *testing.T) {
	root := t.TempDir()
	writeSessionDir(t, root, "case1", map[string]string{
		"log.jsonl": `{"id":"01A","session_name":"case1","time":"2026-01-01T00:00:00Z","type":"x","source":"cli","direction":"internal","summary":"a"}` + "\n" +
			`{"id":"01A","session_name":"case1","time":"2026-01-01T00:00:01Z","type":"x","source":"cli","direction":"internal","summary":"b"}` + "\n",
	})
	if _, err := ReadSessionDir(root, "case1"); err == nil {
		t.Fatal("ReadSessionDir over a duplicate event id must fail")
	}
}

func TestReadSessionDir_RejectsSessionNameMismatch(t *testing.T) {
	root := t.TempDir()
	writeSessionDir(t, root, "case1", map[string]string{
		"log.jsonl": `{"id":"01A","session_name":"other","time":"2026-01-01T00:00:00Z","type":"x","source":"cli","direction":"internal","summary":"a"}` + "\n",
	})
	if _, err := ReadSessionDir(root, "case1"); err == nil {
		t.Fatal("ReadSessionDir over a session_name mismatch must fail")
	}
}

func TestReadSessionDir_RejectsMissingEventID(t *testing.T) {
	root := t.TempDir()
	writeSessionDir(t, root, "case1", map[string]string{
		"log.jsonl": `{"session_name":"case1","time":"2026-01-01T00:00:00Z","type":"x","source":"cli","direction":"internal","summary":"a"}` + "\n",
	})
	if _, err := ReadSessionDir(root, "case1"); err == nil {
		t.Fatal("ReadSessionDir over a record with no id must fail")
	}
}

func TestReadSessionDir_ReportsUnknownFiles(t *testing.T) {
	root := t.TempDir()
	writeSessionDir(t, root, "case1", map[string]string{
		"log.jsonl":       `{"id":"01A","session_name":"case1","time":"2026-01-01T00:00:00Z","type":"x","source":"cli","direction":"internal","summary":"a"}` + "\n",
		"mystery.dat":     "unexpected",
		".cursor.unknown": "0",
	})
	sl, err := ReadSessionDir(root, "case1")
	if err != nil {
		t.Fatalf("ReadSessionDir: %v", err)
	}
	if len(sl.UnknownFiles) != 2 {
		t.Fatalf("UnknownFiles = %v, want 2 entries", sl.UnknownFiles)
	}
}

func TestReadSessionDir_RecognizesButDoesNotImportFileBasedSidecars(t *testing.T) {
	root := t.TempDir()
	writeSessionDir(t, root, "case1", map[string]string{
		"log.jsonl":           `{"id":"01A","session_name":"case1","time":"2026-01-01T00:00:00Z","type":"x","source":"cli","direction":"internal","summary":"a"}` + "\n",
		"tombstone.json":      `{"session_name":"case1"}`,
		"chain_attempts.json": `{}`,
		".lock":               "",
	})
	sl, err := ReadSessionDir(root, "case1")
	if err != nil {
		t.Fatalf("ReadSessionDir: %v", err)
	}
	if len(sl.UnknownFiles) != 0 {
		t.Errorf("UnknownFiles = %v, want none (tombstone/chain_attempts/.lock are recognized, just not imported)", sl.UnknownFiles)
	}
}

func TestSessionLog_TranslateRejectsMidLineAndOutOfRangeOffsets(t *testing.T) {
	root := t.TempDir()
	writeSessionDir(t, root, "case1", map[string]string{
		"log.jsonl": `{"id":"01A","session_name":"case1","time":"2026-01-01T00:00:00Z","type":"x","source":"cli","direction":"internal","summary":"a"}` + "\n",
	})
	sl, err := ReadSessionDir(root, "case1")
	if err != nil {
		t.Fatalf("ReadSessionDir: %v", err)
	}
	if seq, ok := sl.Translate(0); !ok || seq != 1 {
		t.Errorf("Translate(0) = (%d, %v), want (1, true)", seq, ok)
	}
	if _, ok := sl.Translate(-1); ok {
		t.Error("Translate(-1) should reject a negative offset")
	}
	if _, ok := sl.Translate(5); ok {
		t.Error("Translate(5) should reject a mid-line offset")
	}
	if _, ok := sl.Translate(999999); ok {
		t.Error("Translate(999999) should reject an offset beyond the log")
	}
}

func TestListLegacySessionDirs_DecodesPercentEscapedNames(t *testing.T) {
	root := t.TempDir()
	writeSessionDir(t, root, "team/alpha", nil)
	writeSessionDir(t, root, "plain", nil)

	names, err := ListLegacySessionDirs(root)
	if err != nil {
		t.Fatalf("ListLegacySessionDirs: %v", err)
	}
	want := []string{"plain", "team/alpha"}
	if len(names) != len(want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	for i, n := range want {
		if names[i] != n {
			t.Errorf("names[%d] = %q, want %q", i, names[i], n)
		}
	}
}

func TestListLegacySessionDirs_MissingRootIsEmptyNotError(t *testing.T) {
	names, err := ListLegacySessionDirs(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("ListLegacySessionDirs: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("names = %v, want empty", names)
	}
}
