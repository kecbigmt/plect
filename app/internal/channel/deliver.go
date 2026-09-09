// Package channel renders and executes a workflow [[event.channel]]'s delivery
// of one session event to an external runtime. It is the engine the session
// dispatcher drives: the dispatcher resolves which channels match an event and
// supplies the channel inputs (workflow node outputs); this package renders the
// channel definition's primitive fields against {.Event, .Inputs} and runs the
// built-in primitive (unix_socket or exec).
package channel

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kecbigmt/plecture/app/internal/cachehome"
	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/datahome"
	"github.com/kecbigmt/plecture/app/internal/lang"
	"github.com/kecbigmt/plecture/app/internal/plectshim"
	protocol "github.com/kecbigmt/plecture/contracts/channel-protocol"
	"github.com/kecbigmt/plecture/contracts/event"
)

// TerminalResolver resolves one [terminal] verb (attach/capture/send_text/
// send_keys/pid) to the command string a script runs it by, backing a
// `{ terminal = "..." }` capability in a channel's delivery. dir is this
// delivery's private run directory: a verb declared as a shell action is
// materialized there, so it stays runnable exactly as long as the delivery
// that consumes it. Defined here as a plain function type — not imported
// from internal/task — so this package stays free of a dependency on
// internal/task; dispatch (which already depends on both) builds the closure.
type TerminalResolver func(verb, dir string) (string, error)

// BinResolver resolves a channel's `bin` reference to the executable it
// names. Supplied per delivery for the same reason TerminalResolver is: the
// mounted plugins are the caller's knowledge, not this package's.
type BinResolver func(ref string) (string, error)

// eventMap exposes the event with every standard envelope field present
// (empty when unset) so a projection of an unset field resolves to empty
// rather than to absence. Building it explicitly (not via a json round-trip)
// also keeps any future numeric field from being coerced to float64.
func eventMap(ev event.Event) map[string]any {
	md := make(map[string]any, len(ev.Metadata))
	for k, v := range ev.Metadata {
		md[k] = v
	}
	return map[string]any{
		"id":           ev.ID,
		"session_name": ev.SessionName,
		"time":         ev.Time.Format(time.RFC3339Nano),
		"type":         ev.Type,
		"source":       ev.Source,
		"direction":    string(ev.Direction),
		"summary":      ev.Summary,
		"body":         ev.Body,
		"metadata":     md,
	}
}

// deliveryEval is the evaluation this delivery's values resolve against: the
// event and the resolved channel inputs, plus whichever capabilities the
// caller supplied.
func deliveryEval(inputs map[string]any, ev event.Event, opts DeliverOptions, runDir string) lang.Eval {
	if inputs == nil {
		inputs = map[string]any{}
	}
	e := lang.Eval{Roots: lang.Roots{"event": eventMap(ev), "inputs": inputs}}
	if opts.Terminal != nil {
		e.Terminal = func(verb string) (string, error) { return opts.Terminal(verb, runDir) }
	}
	if opts.Bin != nil {
		e.Bin = opts.Bin
	}
	return e
}

// DeliverOptions carries optional per-delivery overrides beyond the channel
// definition and event itself. The zero value is what Deliver uses: no
// capabilities in scope.
type DeliverOptions struct {
	// Terminal resolves this delivery's terminal capabilities; nil means a
	// channel that consumes one gets a clear "not available" error rather
	// than resolving to an empty command.
	Terminal TerminalResolver
	// Bin resolves this delivery's plugin-owned executables; nil means a
	// channel naming one cannot be delivered.
	Bin BinResolver
}

// Deliver renders a channel definition against the event and resolved inputs,
// then performs ONE delivery attempt on the host. The caller resolves inputs
// (node outputs) and owns retry/timeout (see DeliverWithRetry). Equivalent to
// DeliverWithOptions with the zero DeliverOptions.
func Deliver(ctx context.Context, def config.ChannelDefinition, inputs map[string]any, ev event.Event) error {
	return deliver(ctx, def, inputs, ev, DeliverOptions{})
}

// DeliverWithOptions is Deliver with the full set of per-delivery overrides.
func DeliverWithOptions(ctx context.Context, def config.ChannelDefinition, inputs map[string]any, ev event.Event, opts DeliverOptions) error {
	return deliver(ctx, def, inputs, ev, opts)
}

func deliver(ctx context.Context, def config.ChannelDefinition, inputs map[string]any, ev event.Event, opts DeliverOptions) error {
	// One private run directory per delivery, holding whatever the delivery
	// materializes: the binding transport for a shell channel, and a
	// terminal verb declared as a shell action for either variant. A
	// unix_socket delivery runs no process and materializes nothing.
	runDir := ""
	if def.Type != config.ChannelTypeUnixSocket {
		dir, err := os.MkdirTemp("", "plect-channel-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		runDir = dir
	}
	eval := deliveryEval(inputs, ev, opts, runDir)
	switch def.Type {
	case config.ChannelTypeUnixSocket:
		return deliverUnixSocket(ctx, def, eval)
	case config.ChannelTypeExec, config.ChannelTypeShell:
		return deliverProcess(ctx, def, eval, runDir)
	default:
		return fmt.Errorf("unknown channel type %q", def.Type)
	}
}

func deliverUnixSocket(ctx context.Context, def config.ChannelDefinition, eval lang.Eval) error {
	path, _, err := eval.Argument(def.Path)
	if err != nil {
		return fmt.Errorf("channel path: %w", err)
	}
	body, _, err := eval.Bytes(def.Body)
	if err != nil {
		return fmt.Errorf("channel body: %w", err)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return fmt.Errorf("dial %s: %w", path, err)
	}
	defer conn.Close()
	// Bound the write even when the caller passes a deadline-less ctx, so a peer
	// that holds the connection open without reading can't block Deliver forever.
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(fallbackWriteDeadline)
	}
	_ = conn.SetWriteDeadline(deadline)
	// Source stays empty: a non-empty Source lets the delivery daemon treat a
	// "y/n <id>" body as a human permission verdict, so a content event must not
	// set it.
	data, err := protocol.NewEnvelope(protocol.MsgMessage, protocol.MessagePayload{Text: string(body)})
	if err != nil {
		return err
	}
	return writeFramed(conn, data)
}

// maxFrame matches the delivery daemon's readMessage cap. Rejecting here turns an
// oversized payload into a clean error instead of a write that succeeds locally
// but is dropped (and the connection closed) by the server.
const maxFrame = 1 << 20

const fallbackWriteDeadline = 30 * time.Second

func writeFramed(conn net.Conn, data []byte) error {
	if len(data) > maxFrame {
		return fmt.Errorf("channel message %d bytes exceeds %d limit", len(data), maxFrame)
	}
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(data)))
	if _, err := conn.Write(header); err != nil {
		return err
	}
	_, err := conn.Write(data)
	return err
}

// deliverProcess runs an exec or shell channel in the delivery's own run
// directory: the binding transport writes there for a shell channel, and an
// exec delivery touches no filesystem of its own.
func deliverProcess(ctx context.Context, def config.ChannelDefinition, eval lang.Eval, runDir string) error {
	execution, err := eval.Run(filepath.Join(runDir, "action"), def.Action, nil)
	if err != nil {
		return fmt.Errorf("channel %s: %w", def.Type, err)
	}
	cmd := exec.CommandContext(ctx, execution.Argv[0], execution.Argv[1:]...)
	// A channel has no way to bind env explicitly, and no known channel
	// destination depends on inheriting either data-home or cache-home
	// variable, so this is a full, unconditional strip.
	env := cachehome.StripIsolated(datahome.IsolatedEnv())
	if shimDir, shimErr := plectshim.ForCurrentProcess(); shimErr == nil {
		env = plectshim.PatchPath(env, shimDir)
	}
	cmd.Env = env
	if len(execution.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(execution.Stdin)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("exec %s: %w: %s", execution.Argv[0], err, msg)
		}
		return fmt.Errorf("exec %s: %w", execution.Argv[0], err)
	}
	return nil
}
