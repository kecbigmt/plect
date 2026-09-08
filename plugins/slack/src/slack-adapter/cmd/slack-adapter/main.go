package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/kecbigmt/plecture/plugins/slack/src/slack-adapter/internal/adapter"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "subscribe":
			os.Exit(runSubscribeCommand(os.Args[2:], os.Stdout, os.Stderr))
		case "resource":
			os.Exit(runResourceCommand(os.Args[2:], os.Stdout, os.Stderr))
		}
	}
	runServer()
}

// runSubscribeCommand implements `slack-adapter subscribe unbound-mentions`.
// It never opens a Socket Mode connection of its own: it is a client of the
// resident adapter's /unbound-mentions feed, so any number of these can run
// without each consuming another connection to Slack.
func runSubscribeCommand(args []string, out, errOut io.Writer) int {
	if len(args) == 0 || args[0] != "unbound-mentions" {
		fmt.Fprintln(errOut, "usage: slack-adapter subscribe unbound-mentions --base-url <url> --channel-ids <json-array>")
		return 2
	}

	fs := flag.NewFlagSet("subscribe unbound-mentions", flag.ContinueOnError)
	fs.SetOutput(errOut)
	baseURL := fs.String("base-url", "", "resident slack-adapter base URL (required)")
	channelIDsJSON := fs.String("channel-ids", "[]", "JSON array of channel IDs to include")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *baseURL == "" {
		fmt.Fprintln(errOut, "slack-adapter subscribe unbound-mentions: --base-url is required")
		return 2
	}
	var channelIDs []string
	if err := json.Unmarshal([]byte(*channelIDsJSON), &channelIDs); err != nil {
		fmt.Fprintf(errOut, "slack-adapter subscribe unbound-mentions: --channel-ids: %v\n", err)
		return 2
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := adapter.RunSubscribeUnboundMentions(ctx, *baseURL, channelIDs, out); err != nil {
		fmt.Fprintln(errOut, "slack-adapter subscribe unbound-mentions:", err)
		return 1
	}
	return 0
}

// thread has nothing live to report beyond a mention appearance, which
// already flows through query.subscribe, so this prints "{}" rather than
// fabricate a state key.
func runResourceCommand(args []string, out, errOut io.Writer) int {
	if len(args) == 0 || args[0] != "observe" {
		fmt.Fprintln(errOut, "usage: slack-adapter resource observe --resource <url>")
		return 2
	}

	fs := flag.NewFlagSet("resource observe", flag.ContinueOnError)
	fs.SetOutput(errOut)
	resource := fs.String("resource", "", "resource identifier (a Slack thread permalink)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *resource == "" {
		fmt.Fprintln(errOut, "slack-adapter resource observe: --resource is required")
		return 2
	}

	fmt.Fprintln(out, "{}")
	return 0
}

func runServer() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})).
		With("component", "slack-adapter")

	cfg := adapter.LoadConfig()

	if err := cfg.ValidateStartup(); err != nil {
		logger.Error(err.Error())
		os.Exit(1)
	}

	a := adapter.New(cfg, logger)
	defer a.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/info", a.HandleInfo)
	mux.HandleFunc("/threads", a.HandleCreateThread)
	mux.HandleFunc("/messages", a.HandlePostMessage)
	mux.HandleFunc("/subscribe", a.HandleSubscribe)
	mux.HandleFunc("/subscribers", a.HandleSubscribers)
	mux.HandleFunc("/unbound-mentions", a.HandleUnboundMentions)
	mux.HandleFunc("/notify", a.HandleNotify)
	mux.HandleFunc("/status", a.HandleSetStatus)
	mux.HandleFunc("/stream", a.HandleStream)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start HTTP + WebSocket server
	server := &http.Server{Addr: cfg.ListenAddr, Handler: mux}
	go func() {
		logger.Info("listening", "addr", cfg.ListenAddr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http server error", "error", err)
			os.Exit(1)
		}
	}()

	// Start the optional Slack Socket Mode relay.
	go func() {
		if err := a.Run(ctx); err != nil {
			logger.Error("slack socket mode error", "error", err)
			cancel()
		}
	}()

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case <-sigCh:
		logger.Info("shutting down")
	case <-ctx.Done():
	}

	cancel()
	server.Close()
}
