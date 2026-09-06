package service

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/dispatch"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/sessionhub"
	protocol "github.com/kecbigmt/plecture/contracts/channel-protocol"
	"github.com/kecbigmt/plecture/contracts/event"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// startFakeChannelSocket listens on a unix socket and decodes each framed
// message it receives, standing in for a runtime's own delivery endpoint
// the same way app/internal/dispatch's own tests do.
func startFakeChannelSocket(t *testing.T) (string, <-chan protocol.MessagePayload) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	recv := make(chan protocol.MessagePayload, 16)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				header := make([]byte, 4)
				if _, err := io.ReadFull(conn, header); err != nil {
					return
				}
				data := make([]byte, binary.BigEndian.Uint32(header))
				if _, err := io.ReadFull(conn, data); err != nil {
					return
				}
				var env protocol.Envelope
				if json.Unmarshal(data, &env) != nil {
					return
				}
				var pl protocol.MessagePayload
				if env.UnmarshalPayload(&pl) == nil {
					recv <- pl
				}
			}(conn)
		}
	}()
	return path, recv
}

// TestUp_PopulationMemberNodeResultDeliveredThroughChannel is the full-stack
// counterpart of TestUp_PopulationMemberRepairRecordsNodeResultWithoutUpTransition:
// it drives a real dispatch.Supervisor against the session Up produces, and
// asserts the plect.node.result event actually reaches the workflow's
// channel — not just that it lands in the log.
func TestUp_PopulationMemberNodeResultDeliveredThroughChannel(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	sock, recv := startFakeChannelSocket(t)
	store := testStore(t)
	sessionName := "session1"
	cfg := writeWorkflowFixture(t, t.TempDir(), "default",
		[]taskFixture{{id: "agent", scope: "run", setup: fmt.Sprintf(`printf '{"socket_path":"%s"}'`, sock)}},
		[]nodeFixture{{id: "agent"}},
	)
	if err := os.MkdirAll(filepath.Join(cfg.BaseDir, "channels"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.BaseDir, "channels", "claude_channel.toml"), []byte(`
[claude_channel]
kind = "channel"
type = "unix_socket"
path = { from = "inputs.path" }
body = { json = { from = "event" } }

[claude_channel.input_schema]
path = { type = "string", required = true }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	addWorkflowFields(t, cfg, "default", `
[[default.event.channel]]
name    = "runtime"
uses    = "claude_channel"
include = ["plect.node.result"]

[default.event.channel.inputs]
path = { from = "nodes.agent.outputs.socket_path" }
`)

	seedSession(t, store, sessionName, "acct", 1, "default", map[string]*contract.TaskState{})
	if err := store.Update(sessionName, func(s *domain.Session) error {
		s.Population = &contract.PopulationProvenance{Workflow: "default", Name: "pop"}
		return nil
	}); err != nil {
		t.Fatalf("set population provenance: %v", err)
	}

	log := eventlog.NewStore(store.Dir())
	// Seed the dispatcher's cursor at the empty tail before Up ever appends
	// anything — the same ordering service.Create relies on for its own
	// initial-instruction delivery — so the supervisor's dispatcher, started
	// after Up below, still delivers the event Up appends before it starts.
	dispatch.SeedCursor(log, sessionName)

	// No Observer: mirrors internal/population/runtime.go's real repair call.
	if _, err := Up(cfg, store, UpParams{Identifier: sessionName}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	hub := sessionhub.NewRegistry(log)
	defer hub.Close()
	sup := dispatch.NewSupervisor(func() *config.Config { return cfg }, store, log, hub)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)

	select {
	case pl := <-recv:
		var ev map[string]any
		if err := json.Unmarshal([]byte(pl.Text), &ev); err != nil {
			t.Fatalf("payload is not event json: %v", err)
		}
		if ev["type"] != event.TypeNodeResult {
			t.Fatalf("delivered type = %v, want %q", ev["type"], event.TypeNodeResult)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("plect.node.result was not delivered to the channel")
	}
}
