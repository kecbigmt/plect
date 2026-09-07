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
	"github.com/kecbigmt/plecture/app/internal/sockettest"
	"github.com/kecbigmt/plecture/app/internal/state"
	protocol "github.com/kecbigmt/plecture/contracts/channel-protocol"
	"github.com/kecbigmt/plecture/contracts/event"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

func startFakeChannelSocket(t *testing.T) (string, <-chan protocol.MessagePayload) {
	t.Helper()
	path := filepath.Join(sockettest.Dir(t), "c.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	recv := make(chan protocol.MessagePayload, 16)
	go func() {
		// Reads each connection to completion before accepting the next: a goroutine per connection would let two race to push to recv out of send order.
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			func(conn net.Conn) {
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

func TestUp_PopulationMemberInPlaceRepairDeliversNodeResultThroughChannel(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	sock, recv := startFakeChannelSocket(t)
	store := testStore(t)
	sessionName := "session1"
	cfg := writeWorkflowFixture(t, t.TempDir(), "default",
		[]taskFixture{{
			id:      "agent",
			scope:   "run",
			setup:   fmt.Sprintf(`printf '{"socket_path":"%s"}'`, sock),
			alive:   "false",
			cleanup: "true",
		}},
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

	seedSessionWithNodes(t, store, sessionName, "acct", 1, "default", map[string]*contract.TaskState{
		"agent": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, Outputs: map[string]any{"socket_path": sock}},
	})
	if err := store.UpdatePopulation("default/pop", func(population *state.PopulationState) error {
		population.Workflow = "default"
		population.Name = "pop"
		return nil
	}); err != nil {
		t.Fatalf("seed population: %v", err)
	}
	if err := store.Update(sessionName, func(s *domain.Session) error {
		s.Population = &contract.PopulationProvenance{Workflow: "default", Name: "pop"}
		return nil
	}); err != nil {
		t.Fatalf("set population provenance: %v", err)
	}

	log := eventlog.NewStore(store.Dir())
	dispatch.SeedCursor(log, sessionName)

	if _, err := Up(cfg, store, UpParams{Identifier: sessionName}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if upEvents, _, _, err := log.List(sessionName, 0, event.Filter{Types: []string{event.TypeWorkflowPopulationUp}}); err != nil {
		t.Fatalf("list workflow_population.up events: %v", err)
	} else if len(upEvents) != 0 {
		t.Fatalf("in-place repair recorded a population up transition: %+v", upEvents)
	}

	hub := sessionhub.NewRegistry(log)
	defer hub.Close()
	sup := dispatch.NewSupervisor(func() *config.Config { return cfg }, store, log, hub)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)

	wantActions := []string{event.NodeResultActionAlive, event.NodeResultActionCleanup, event.NodeResultActionSetup}
	for i, want := range wantActions {
		select {
		case pl := <-recv:
			var ev map[string]any
			if err := json.Unmarshal([]byte(pl.Text), &ev); err != nil {
				t.Fatalf("delivery %d: payload is not event json: %v", i, err)
			}
			if ev["type"] != event.TypeNodeResult {
				t.Fatalf("delivery %d type = %v, want %q", i, ev["type"], event.TypeNodeResult)
			}
			md, _ := ev["metadata"].(map[string]any)
			if md["action"] != want {
				t.Fatalf("delivery %d action = %v, want %q", i, md["action"], want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("delivery %d (action=%s) was not received", i, want)
		}
	}
}
