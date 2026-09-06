package commands

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/dispatch"
	"github.com/kecbigmt/plecture/app/internal/eventbus"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/pluginservice"
	"github.com/kecbigmt/plecture/app/internal/population"
	"github.com/kecbigmt/plecture/app/internal/reactor"
	"github.com/kecbigmt/plecture/app/internal/sessionhub"
	"github.com/kecbigmt/plecture/app/internal/state"
)

var serveEventBusSocket string

// liveConfigRefresh bounds how stale the daemon's view of mounted plugins
// can get. Kept well above the 1s reconcile poll: resolving plugins hashes
// every enabled plugin's full directory tree (see plugins.VerifyAndMountAll),
// so running it every reconcile tick would turn a cheap poll into an
// increasingly expensive one as the plugin set grows.
const liveConfigRefresh = 30 * time.Second

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the resident daemon",
	Long: `Run the resident daemon that hosts Plecture's local runtime followers.

It serves the durable event log over a Unix domain socket as an HTTP/SSE API:
POST /v1/events (append), GET /v1/events (list), GET /v1/stream (SSE replay+live).

The socket is created 0600, so same-user processes need no token; set
PLECT_BUS_TOKEN to also require a bearer token (e.g. when proxied to a browser).

Config (including which plugins are mounted) is re-resolved periodically, so
a plugin enabled after this command started becomes visible without a
restart, within one refresh interval.`,
	Args: cobra.NoArgs,
	// No persistence.EnsureCurrent call here: root.go's PersistentPreRunE
	// already ran it, since this command has no PersistentPreRunE of its own.
	RunE: func(cmd *cobra.Command, _ []string) error {
		socket := serveEventBusSocket
		if socket == "" {
			socket = defaultBusSocket()
		}
		if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
			return fmt.Errorf("create socket dir: %w", err)
		}
		// A stale socket from a previous run would make Listen fail with EADDRINUSE.
		_ = os.Remove(socket)
		ln, err := net.Listen("unix", socket)
		if err != nil {
			return fmt.Errorf("listen on %s: %w", socket, err)
		}
		defer func() { ln.Close(); os.Remove(socket) }()
		if err := os.Chmod(socket, 0o600); err != nil {
			return fmt.Errorf("chmod socket: %w", err)
		}

		// Resolve the log dir the same way writers do (eventlog.NewStore("") =>
		// $XDG_DATA_HOME/plect/events, matching service.EventPublish). Log it so a
		// daemon/writer env mismatch (different XDG_DATA_HOME) is visible rather
		// than silently appending to a different tree.
		store := eventlog.NewStore("")
		// One per-session reader shared by SSE subscribers (and, with the channel
		// dispatcher onto it, the single follow loop per session).
		hub := sessionhub.NewRegistry(store)
		srv := eventbus.New(store, os.Getenv("PLECT_BUS_TOKEN"), hub)
		httpSrv := &http.Server{
			Handler:           srv.Routes(),
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       0, // SSE connections are intentionally long-lived
		}

		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		// A one-shot config.Load() would pin the plugin-mount layer to
		// whatever existed at process start for the daemon's whole
		// lifetime — every other consumer is a short-lived CLI invocation
		// that reloads fresh per run, but a daemon has no equivalent
		// per-invocation boundary. Live re-resolves on liveConfigRefresh so
		// a plugin mounted after the daemon started is visible on the next
		// session up transition, not just after a daemon restart.
		live, err := config.NewLive(config.Load, liveConfigRefresh)
		if err != nil {
			return err
		}
		stateStore := state.NewStore("")

		// Session dispatchers share this process's event log and session state:
		// one per active session delivers events to its workflow-declared channels.
		sup := dispatch.NewSupervisor(live.Get, stateStore, store, hub)
		var supWG sync.WaitGroup
		supWG.Go(func() { sup.Run(ctx) })

		// The tick reactor is dispatch's sibling: one per active session,
		// ticking on a declared `[tick]` pattern, the judge builtin, or a
		// `heartbeat` sweep (docs/wiki/verification-gate.md), instead of
		// leaving `plect tick` to an orchestrator's judgment or memory.
		react := reactor.NewSupervisor(live.Get, stateStore, store, hub)
		var reactWG sync.WaitGroup
		reactWG.Go(func() { react.Run(ctx) })

		populations := population.NewSupervisor(live.Get, stateStore, store)
		var populationWG sync.WaitGroup
		populationWG.Go(func() { populations.Run(ctx) })

		go live.Run(ctx)

		// Plugin-declared [[services]] (a plugin-owned daemon such as a chat
		// relay or an issue-tracker watcher) are supervised the same way:
		// started and stopped with the resident process, restarted on crash
		// or on the owning plugin's content change. Polls config.LoadPlugins() itself rather
		// than live.Get()/cfg.Plugins: services only need the mounted
		// plugin list and lockfile, not a full re-resolved Config, and
		// polling independently of live's 30s refresh keeps a plugin
		// content change (a `plect plugin update` while the resident process is
		// running) picked up on the supervisor's own, tighter poll cycle.
		svcSup := pluginservice.NewSupervisor(config.LoadPlugins)
		var svcWG sync.WaitGroup
		svcWG.Go(func() { svcSup.Run(ctx) })

		go func() {
			<-ctx.Done()
			_ = httpSrv.Close()
		}()

		fmt.Fprintf(cmd.ErrOrStderr(), "plect serve listening on %s (events: %s)\n", socket, store.Root())
		serveErr := httpSrv.Serve(ln)
		stop()         // cancel ctx so the supervisors tear down even if Serve failed without a signal
		supWG.Wait()   // let the dispatch supervisor cancel and join its dispatchers
		reactWG.Wait() // let the reactor supervisor cancel and join its reactors
		populationWG.Wait()
		svcWG.Wait() // let the service supervisor stop every running plugin service
		hub.Close()  // cancel any reader still alive after subscribers/dispatchers/reactors left
		if serveErr != nil && serveErr != http.ErrServerClosed {
			return serveErr
		}
		return nil
	},
}

// defaultBusSocket mirrors the plect-mcp convention (%t/plect/... = $XDG_RUNTIME_DIR).
func defaultBusSocket() string {
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		return filepath.Join(rt, "plect", "bus.sock")
	}
	return filepath.Join(os.TempDir(), "plect", "bus.sock")
}

func init() {
	serveCmd.Flags().StringVar(&serveEventBusSocket, "socket", "", "Unix socket path (default $XDG_RUNTIME_DIR/plect/bus.sock)")
	rootCmd.AddCommand(serveCmd)
}
