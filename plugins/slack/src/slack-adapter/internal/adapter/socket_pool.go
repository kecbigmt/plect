package adapter

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/kecbigmt/plecture/contracts/channel-protocol"
)

// SocketPool manages connections to channel-server instances via Unix socket.
// Connections are keyed by socket_path and reused across messages.
type SocketPool struct {
	mu     sync.RWMutex
	conns  map[string]*socketConn // socket_path -> connection
	poster ThreadPoster
	logger *slog.Logger
	// capture, if set, records an outbound message (Claude reply / permission
	// prompt) the channel-server sent back, keyed by thread_ts. Optional (nil in
	// tests that don't exercise capture).
	capture func(threadTS, eventType, body string)
	// statusMgr, if set, clears the thread's shimmer status once an outbound
	// message actually reaches Slack. Optional (nil in tests that don't
	// exercise status).
	statusMgr *StatusManager
}

// socketConn holds a connection and its current registration.
type socketConn struct {
	client    *SocketClient
	channelID string
	threadTS  string
}

// ThreadPoster posts messages to Slack threads.
type ThreadPoster interface {
	PostToThread(channelID, threadTS, text string) (string, error)
	ThreadStatusSetter
}

// ThreadCreator creates Slack thread roots and returns their canonical URL.
type ThreadCreator interface {
	CreateThread(channelID, text string) (threadTS, permalink string, err error)
}

// NewSocketPool creates a new socket connection pool.
func NewSocketPool(poster ThreadPoster, logger *slog.Logger, capture func(threadTS, eventType, body string), statusMgr *StatusManager) *SocketPool {
	return &SocketPool{
		conns:     make(map[string]*socketConn),
		poster:    poster,
		logger:    logger,
		capture:   capture,
		statusMgr: statusMgr,
	}
}

// Send sends a message to the channel-server at the given socket path.
// It reuses an existing connection or creates a new one.
func (sr *SocketPool) Send(socketPath, channelID string, msg protocol.MessagePayload) error {
	conn, err := sr.getOrConnect(socketPath, channelID, msg.ThreadTS)
	if err != nil {
		return err
	}

	if err := conn.client.SendMessage(msg); err != nil {
		sr.logger.Error("send failed, removing connection", "socket_path", socketPath, "error", err)
		sr.mu.Lock()
		if cur, ok := sr.conns[socketPath]; ok && cur == conn {
			conn.client.Close()
			delete(sr.conns, socketPath)
		}
		sr.mu.Unlock()
		return err
	}
	return nil
}

// getOrConnect returns an existing connection or creates a new one.
func (sr *SocketPool) getOrConnect(socketPath, channelID, threadTS string) (*socketConn, error) {
	sr.mu.Lock()
	defer sr.mu.Unlock()

	if conn, ok := sr.conns[socketPath]; ok {
		if conn.threadTS == threadTS && conn.channelID == channelID {
			return conn, nil
		}
		// Otherwise it's still registered for its old thread/channel.
		if err := conn.client.Rebind(threadTS, channelID); err != nil {
			sr.logger.Warn("rebind failed, reconnecting", "socket_path", socketPath, "error", err)
			conn.client.Close()
			delete(sr.conns, socketPath)
		} else {
			conn.threadTS = threadTS
			conn.channelID = channelID
			sr.logger.Info("rebound connection", "socket_path", socketPath, "thread_ts", threadTS, "channel_id", channelID)
			return conn, nil
		}
	}

	client, err := NewSocketClient(socketPath, threadTS, channelID, sr.logger,
		func(reply protocol.ReplyPayload) {
			channelID := sr.channelIDFor(socketPath)
			if _, err := sr.poster.PostToThread(channelID, reply.ThreadTS, reply.Text); err != nil {
				sr.logger.Error("failed to post reply to Slack", "error", err)
				return // don't log an outbound event that never reached Slack
			}
			if sr.capture != nil {
				sr.capture(reply.ThreadTS, "claude.reply", reply.Text)
			}
			sr.clearStatus(channelID, reply.ThreadTS)
		},
		func(perm protocol.PermissionPayload) {
			channelID := sr.channelIDFor(socketPath)
			if _, err := sr.poster.PostToThread(channelID, perm.ThreadTS, perm.Text); err != nil {
				sr.logger.Error("failed to post permission to Slack", "error", err)
				return // don't log an outbound event that never reached Slack
			}
			if sr.capture != nil {
				sr.capture(perm.ThreadTS, "claude.permission_request", perm.Text)
			}
			sr.clearStatus(channelID, perm.ThreadTS)
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to channel-server: %w", err)
	}

	conn := &socketConn{client: client, channelID: channelID, threadTS: threadTS}
	sr.conns[socketPath] = conn

	// Start read loop in background
	go func() {
		client.ReadLoop()
		sr.mu.Lock()
		if cur, ok := sr.conns[socketPath]; ok && cur == conn {
			delete(sr.conns, socketPath)
			sr.logger.Info("connection closed", "socket_path", socketPath)
		}
		sr.mu.Unlock()
	}()

	sr.logger.Info("connected", "socket_path", socketPath)
	return conn, nil
}

// channelIDFor is looked up per-message rather than captured in a closure
// at connect time, since Rebind can change it later.
func (sr *SocketPool) channelIDFor(socketPath string) string {
	sr.mu.RLock()
	defer sr.mu.RUnlock()
	if conn, ok := sr.conns[socketPath]; ok {
		return conn.channelID
	}
	return ""
}

// clearStatus doesn't rely on Slack's own auto-clear on an app reply,
// which wouldn't cover the permission-prompt path anyway (not a "reply"
// from Slack's perspective).
func (sr *SocketPool) clearStatus(channelID, threadTS string) {
	if sr.statusMgr == nil {
		return
	}
	if err := sr.statusMgr.Clear(channelID, threadTS); err != nil {
		sr.logger.Error("failed to clear Slack thread status", "channel_id", channelID, "thread_ts", threadTS, "error", err)
	}
}

// Connect establishes a connection to a channel-server socket without sending a message.
// Used for pre-warming connections on startup.
func (sr *SocketPool) Connect(socketPath, channelID, threadTS string) error {
	_, err := sr.getOrConnect(socketPath, channelID, threadTS)
	return err
}

// Close disconnects all clients.
func (sr *SocketPool) Close() {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	for path, conn := range sr.conns {
		conn.client.Close()
		delete(sr.conns, path)
	}
}
