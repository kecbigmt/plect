package channel

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/contracts/event"
)

// channelDef loads one channel declaration the way a real config home does,
// so these tests exercise the declarations an author writes rather than a
// hand-built runtime struct.
func channelDef(t *testing.T, body string) config.ChannelDefinition {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "channels", "test.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	defs, err := (&config.Config{BaseDir: dir}).LoadChannels()
	if err != nil {
		t.Fatalf("load channel: %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("loaded %d channels, want 1", len(defs))
	}
	for _, def := range defs {
		return def
	}
	panic("unreachable")
}

func readFramed(conn net.Conn) ([]byte, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, err
	}
	data := make([]byte, binary.BigEndian.Uint32(header))
	if _, err := io.ReadFull(conn, data); err != nil {
		return nil, err
	}
	return data, nil
}

func TestDeliver_UnixSocket(t *testing.T) {
	def := channelDef(t, `
[c]
kind = "channel"
type = "unix_socket"
path = { from = "inputs.path" }
body = { json = { from = "event" } }

[c.input_schema]
path = { type = "string", required = true }
`)
	sock := filepath.Join(t.TempDir(), "c.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	got := make(chan []byte, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		data, err := readFramed(conn)
		if err != nil {
			return
		}
		got <- data
	}()

	ev := event.Event{Type: event.TypeUserEmit, Body: "hello"}
	if err := Deliver(context.Background(), def, map[string]any{"path": sock}, ev); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	data := <-got
	var envelope struct {
		Payload struct{ Text string } `json:"payload"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("envelope: %v (%s)", err, data)
	}
	var carried map[string]any
	if err := json.Unmarshal([]byte(envelope.Payload.Text), &carried); err != nil {
		t.Fatalf("payload is not the event as JSON: %v (%s)", err, envelope.Payload.Text)
	}
	if carried["body"] != "hello" {
		t.Errorf("carried body = %v, want hello", carried["body"])
	}
}

func TestDeliver_Exec(t *testing.T) {
	dir := t.TempDir()
	def := channelDef(t, `
[c]
kind    = "channel"
type    = "exec"
command = "touch"
args    = [{ expr = "inputs.dir + '/' + event.type" }]

[c.input_schema]
dir = { type = "string", required = true }
`)
	ev := event.Event{Type: event.TypeInstruction}
	if err := Deliver(context.Background(), def, map[string]any{"dir": dir}, ev); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, event.TypeInstruction)); err != nil {
		t.Errorf("expected touched file: %v", err)
	}
}

// An exec channel may hand its payload to the process's standard input
// instead of its argv, which keeps a large or sensitive body out of the
// process table.
func TestDeliver_ExecStdin(t *testing.T) {
	out := filepath.Join(t.TempDir(), "stdin.out")
	def := channelDef(t, `
[c]
kind    = "channel"
type    = "exec"
command = "sh"
args    = ["-c", 'cat > "$1"', "channel", { from = "inputs.out" }]
stdin   = { json = { from = "event" } }

[c.input_schema]
out = { type = "string", required = true }
`)
	ev := event.Event{Type: event.TypeUserEmit, Body: "piped"}
	if err := Deliver(context.Background(), def, map[string]any{"out": out}, ev); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var carried map[string]any
	if err := json.Unmarshal(raw, &carried); err != nil {
		t.Fatalf("stdin is not the event as JSON: %v (%s)", err, raw)
	}
	if carried["body"] != "piped" {
		t.Errorf("carried body = %v, want piped", carried["body"])
	}
}

// A shell channel's script is literal: the event reaches it through the
// binding transport, never as part of the command.
func TestDeliver_Shell(t *testing.T) {
	out := filepath.Join(t.TempDir(), "shell.out")
	def := channelDef(t, `
[c]
kind   = "channel"
type   = "shell"
script = 'printf "%s" "$message" > "$out"'

[c.bind]
out     = { from = "inputs.out" }
message = { expr = "'[' + event.type + '] ' + event.body" }

[c.input_schema]
out = { type = "string", required = true }
`)
	ev := event.Event{Type: event.TypeUserEmit, Body: `a "quoted" $body; rm -rf /`}
	if err := Deliver(context.Background(), def, map[string]any{"out": out}, ev); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if want := "[" + ev.Type + "] " + ev.Body; string(raw) != want {
		t.Errorf("script saw %q, want %q — a bound value is data, not command text", raw, want)
	}
}

func TestDeliver_ProcessStripsBothDataHomeVars(t *testing.T) {
	t.Setenv("PLECT_DATA_HOME", "/poisoned")
	t.Setenv("XDG_DATA_HOME", "/poisoned-xdg")

	out := filepath.Join(t.TempDir(), "env.out")
	def := channelDef(t, `
[c]
kind   = "channel"
type   = "shell"
script = 'env > "$out"'

[c.bind]
out = { from = "inputs.out" }

[c.input_schema]
out = { type = "string", required = true }
`)
	ev := event.Event{Type: event.TypeUserEmit}
	if err := Deliver(context.Background(), def, map[string]any{"out": out}, ev); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "PLECT_DATA_HOME=") || strings.Contains(string(raw), "XDG_DATA_HOME=") {
		t.Fatalf("channel command's env leaked a data-home variable:\n%s", raw)
	}
}

func TestDeliverWithOptions_TerminalCapabilityReachesTheScript(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	def := channelDef(t, `
[c]
kind   = "channel"
type   = "shell"
script = 'sh -c "$send" channel-send "$message" > "$out"'

[c.bind]
send    = { terminal = "send_text" }
message = { from = "event.body" }
out     = { from = "inputs.out" }

[c.input_schema]
out = { type = "string", required = true }
`)
	terminal := func(verb, _ string) (string, error) {
		if verb != "send_text" {
			t.Fatalf("resolver called with verb %q, want send_text", verb)
		}
		return `printf '%s' "$1"`, nil
	}
	ev := event.Event{Type: event.TypeInstruction, Body: "typed"}
	err := DeliverWithOptions(context.Background(), def, map[string]any{"out": out}, ev, DeliverOptions{Terminal: terminal})
	if err != nil {
		t.Fatalf("DeliverWithOptions: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "typed" {
		t.Errorf("output = %q, want the terminal command to have carried the body", raw)
	}
}

// A channel consuming a terminal capability with no resolver in scope must
// fail the delivery, not run against an empty command.
func TestDeliver_TerminalWithNoResolverFailsLoud(t *testing.T) {
	def := channelDef(t, `
[c]
kind   = "channel"
type   = "shell"
script = 'true'

[c.bind]
send = { terminal = "send_text" }
`)
	err := Deliver(context.Background(), def, nil, event.Event{Type: event.TypeInstruction})
	if err == nil || !strings.Contains(err.Error(), "send_text") {
		t.Fatalf("expected a terminal-unavailable error naming send_text, got %v", err)
	}
}

// An event's optional field is present-and-empty rather than absent, so a
// projection of an unset body resolves to empty instead of failing.
func TestDeliver_EmptyOptionalEventFieldResolvesEmpty(t *testing.T) {
	dir := t.TempDir()
	def := channelDef(t, `
[c]
kind    = "channel"
type    = "exec"
command = "touch"
args    = [{ expr = "inputs.dir + '/got-' + event.body" }]

[c.input_schema]
dir = { type = "string", required = true }
`)
	ev := event.Event{Type: "example.status", Summary: "CI failed"} // Body empty
	if err := Deliver(context.Background(), def, map[string]any{"dir": dir}, ev); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "got-")); err != nil {
		t.Errorf("expected file from the empty-field resolution: %v", err)
	}
}

func TestDeliver_ExecFailureCarriesStderr(t *testing.T) {
	def := channelDef(t, `
[c]
kind    = "channel"
type    = "exec"
command = "sh"
args    = ["-c", "echo boom >&2; exit 3"]
`)
	err := Deliver(context.Background(), def, nil, event.Event{Type: event.TypeInstruction})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected the failure to carry stderr, got %v", err)
	}
}

// An unwired input has nothing to resolve, and delivery fails rather than
// sending an empty value the receiver would read as real.
func TestDeliver_MissingInputFailsDelivery(t *testing.T) {
	def := channelDef(t, `
[c]
kind    = "channel"
type    = "exec"
command = "true"
args    = [{ from = "inputs.absent" }]

[c.input_schema]
absent = { type = "string" }
`)
	err := Deliver(context.Background(), def, nil, event.Event{Type: event.TypeInstruction})
	if err == nil || !strings.Contains(err.Error(), "inputs.absent") {
		t.Fatalf("expected an unresolved-input error, got %v", err)
	}
}

func TestDeliver_UnknownType(t *testing.T) {
	def := config.ChannelDefinition{Type: "carrier-pigeon"}
	if err := Deliver(context.Background(), def, nil, event.Event{Type: event.TypeInstruction}); err == nil {
		t.Fatal("expected an unknown-type error")
	}
}
