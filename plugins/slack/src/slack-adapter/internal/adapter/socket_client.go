package adapter

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"

	"github.com/kecbigmt/plecture/contracts/channel-protocol"
)

// SocketClient connects to a channel-server Unix socket and exchanges messages.
type SocketClient struct {
	conn     net.Conn
	threadTS string
	mu       sync.Mutex
	logger   *slog.Logger
	onReply  func(protocol.ReplyPayload)
	onPerm   func(protocol.PermissionPayload)
}

// NewSocketClient connects to a channel-server Unix socket and registers for a thread.
func NewSocketClient(socketPath, threadTS, channelID string, logger *slog.Logger,
	onReply func(protocol.ReplyPayload),
	onPerm func(protocol.PermissionPayload),
) (*SocketClient, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to channel-server socket: %w", err)
	}

	c := &SocketClient{
		conn:    conn,
		logger:  logger,
		onReply: onReply,
		onPerm:  onPerm,
	}

	if err := c.register(threadTS, channelID); err != nil {
		conn.Close()
		return nil, err
	}

	return c, nil
}

func (c *SocketClient) register(threadTS, channelID string) error {
	reg := protocol.RegisterPayload{
		ThreadTS:  threadTS,
		ChannelID: channelID,
	}
	data, err := protocol.NewEnvelope(protocol.MsgRegister, reg)
	if err != nil {
		return err
	}
	if err := c.writeMessage(data); err != nil {
		return err
	}
	c.mu.Lock()
	c.threadTS = threadTS
	c.mu.Unlock()
	return nil
}

// Rebind re-registers an already-open connection under a different
// thread/channel, so reusing it doesn't leave channel-server pointed at
// the thread it was originally opened for.
func (c *SocketClient) Rebind(threadTS, channelID string) error {
	return c.register(threadTS, channelID)
}

// ReadLoop reads messages from channel-server. Blocks until connection closes.
func (c *SocketClient) ReadLoop() {
	defer c.conn.Close()
	for {
		data, err := c.readMessage()
		if err != nil {
			if err != io.EOF {
				c.logger.Error("read error", "error", err)
			}
			return
		}

		var env protocol.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}

		switch env.Type {
		case protocol.MsgReply:
			var reply protocol.ReplyPayload
			if err := json.Unmarshal(env.Payload, &reply); err != nil {
				continue
			}
			if c.onReply != nil {
				c.onReply(reply)
			}
		case protocol.MsgPermission:
			var perm protocol.PermissionPayload
			if err := json.Unmarshal(env.Payload, &perm); err != nil {
				continue
			}
			if c.onPerm != nil {
				c.onPerm(perm)
			}
		}
	}
}

// SendMessage sends a Slack message to channel-server via the socket.
func (c *SocketClient) SendMessage(msg protocol.MessagePayload) error {
	data, err := protocol.NewEnvelope(protocol.MsgMessage, msg)
	if err != nil {
		return err
	}
	return c.writeMessage(data)
}

// Close closes the socket connection.
func (c *SocketClient) Close() error {
	return c.conn.Close()
}

func (c *SocketClient) writeMessage(data []byte) error {
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(data)))
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.conn.Write(header); err != nil {
		return err
	}
	_, err := c.conn.Write(data)
	return err
}

func (c *SocketClient) readMessage() ([]byte, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(c.conn, header); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(header)
	if length > 1<<20 {
		return nil, fmt.Errorf("message too large: %d bytes", length)
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(c.conn, data); err != nil {
		return nil, err
	}
	return data, nil
}
