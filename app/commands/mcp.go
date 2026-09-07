package commands

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"

	"github.com/kecbigmt/plecture/app/internal/mcpserver"
	"github.com/kecbigmt/plecture/app/internal/service"
)

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "MCP server commands",
}

var mcpServeCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start MCP server on stdio (stdin/stdout)",
	RunE: func(cmd *cobra.Command, args []string) error {
		s := mcpserver.NewServer()
		return server.ServeStdio(s)
	},
}

var (
	mcpListenSocket  string
	mcpListenSession string
)

var mcpListenCmd = &cobra.Command{
	Use:   "listen",
	Short: "Listen on a Unix socket, spawning a stdio MCP session per connection",
	RunE: func(cmd *cobra.Command, args []string) error {
		// --session injects PLECT_SESSION_GUARD into every spawned connection
		// rather than relying on whatever the listener process itself
		// inherited, and derives --socket's default so a task setup script
		// needn't compute one itself.
		var guard string
		socketPath := mcpListenSocket
		if mcpListenSession != "" {
			g, err := service.SessionGuardForOwnSession(mcpListenSession)
			if err != nil {
				return err
			}
			guard = g
			if socketPath == "" {
				path, err := resolveSessionSocket(mcpListenSession, fallbackRuntimeSocketRoot())
				if err != nil {
					return err
				}
				socketPath = path
			}
		}
		if socketPath == "" {
			return fmt.Errorf("--socket is required (or pass --session to derive the per-session default)")
		}

		if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
			return fmt.Errorf("failed to create socket directory: %w", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Handle shutdown signals
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)
		go func() {
			<-sigChan
			fmt.Fprintf(os.Stderr, "Shutting down...\n")
			cancel()
		}()

		// Find our own executable path for spawning child processes
		self, err := os.Executable()
		if err != nil {
			return fmt.Errorf("failed to find own executable: %w", err)
		}

		return mcpserver.Listen(ctx, socketPath, self, guard)
	},
}

// resolveSessionSocket bundles the fallback decision with hardening its root.
func resolveSessionSocket(sessionName, fallbackRoot string) (string, error) {
	path, needsPrivateRoot := defaultSessionMcpListenSocket(sessionName, fallbackRoot)
	if needsPrivateRoot {
		if err := ensurePrivateFallbackRoot(fallbackRoot); err != nil {
			return "", err
		}
	}
	return path, nil
}

// defaultSessionMcpListenSocket derives a per-session socket path under
// $XDG_RUNTIME_DIR, falling back to fallbackRoot instead of the too-long
// os.TempDir(); needsPrivateRoot reports that case.
func defaultSessionMcpListenSocket(sessionName, fallbackRoot string) (path string, needsPrivateRoot bool) {
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		return filepath.Join(rt, "plect-mcp", sessionName+".sock"), false
	}
	return filepath.Join(fallbackRoot, sessionName+".sock"), true
}

// fallbackRuntimeSocketRoot embeds the current uid; a bare shared path has no OS-enforced privacy guarantee.
func fallbackRuntimeSocketRoot() string {
	return fmt.Sprintf("/tmp/plect-mcp-%d", os.Getuid())
}

// ensurePrivateFallbackRoot creates root 0700, or verifies a pre-existing
// one is owned by the caller with no group/other permission bits.
func ensurePrivateFallbackRoot(root string) error {
	if err := os.Mkdir(root, 0o700); err == nil || !os.IsExist(err) {
		if err != nil {
			return fmt.Errorf("create private socket directory %s: %w", root, err)
		}
		return nil
	}
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("stat private socket directory %s: %w", root, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("private socket directory %s is a symlink, refusing to use it", root)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s exists and is not a directory", root)
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("private socket directory %s has permissions %o, want 0700", root, info.Mode().Perm())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("private socket directory %s: cannot verify its owner on this platform", root)
	}
	return checkPrivateDirOwner(root, stat.Uid)
}

// checkPrivateDirOwner is split out for testability: chown needs privileges a test lacks.
func checkPrivateDirOwner(root string, ownerUID uint32) error {
	if ownerUID != uint32(os.Getuid()) {
		return fmt.Errorf("private socket directory %s is owned by uid %d, not the current user", root, ownerUID)
	}
	return nil
}

func init() {
	mcpListenCmd.Flags().StringVar(&mcpListenSocket, "socket", "", "Unix socket path to listen on (default: the --session convention path)")
	mcpListenCmd.Flags().StringVar(&mcpListenSession, "session", "", "Scope this socket to a session: injects PLECT_SESSION_GUARD for it and its own name-space into every spawned connection, and derives --socket's default")

	mcpCmd.AddCommand(mcpServeCmd)
	mcpCmd.AddCommand(mcpListenCmd)
	rootCmd.AddCommand(mcpCmd)
}
