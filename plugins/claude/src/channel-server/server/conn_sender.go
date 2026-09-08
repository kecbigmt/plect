package server

import (
	"fmt"
	"net"
	"sync"

	"github.com/kecbigmt/plecture/contracts/channel-protocol"
)

// ConnSenderStore is a MessageSender that sends permission prompts via a Unix
// socket connection. The connection is set dynamically when a client registers.
type ConnSenderStore struct {
	mu       sync.RWMutex
	conn     net.Conn
	threadTS string
}

// NewConnSenderStore creates a new ConnSenderStore.
func NewConnSenderStore() *ConnSenderStore {
	return &ConnSenderStore{}
}

// SetConn sets the active connection and thread context for sending prompts.
func (s *ConnSenderStore) SetConn(conn net.Conn, threadTS string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conn = conn
	s.threadTS = threadTS
}

// SendPermissionPrompt sends a permission prompt via the stored connection.
func (s *ConnSenderStore) SendPermissionPrompt(text string) error {
	s.mu.RLock()
	conn := s.conn
	threadTS := s.threadTS
	s.mu.RUnlock()

	if conn == nil {
		return fmt.Errorf("no active connection")
	}

	data, err := protocol.NewEnvelope(protocol.MsgPermission, protocol.PermissionPayload{
		Text:     text,
		ThreadTS: threadTS,
	})
	if err != nil {
		return err
	}
	return writeMessage(conn, data)
}
