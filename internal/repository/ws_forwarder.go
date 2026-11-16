package repository

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"

	rafttransport "github.com/disaster37/go-raft/transport"
	"github.com/gorilla/websocket"
)

// WSConn is the minimal bidirectional WebSocket surface used by the
// follower→leader relay.
type WSConn interface {
	ReadMessage() (int, []byte, error)
	WriteMessage(int, []byte) error
	Close() error
}

// WSLeaderDialer dials the leader's internal control listener over mTLS.
type WSLeaderDialer interface {
	Dial(ctx context.Context, rawURL, serverName string, headers map[string][]string) (WSConn, error)
}

// GorillaWSLeaderDialer implements WSLeaderDialer with gorilla/websocket.
type GorillaWSLeaderDialer struct {
	provider *rafttransport.DynamicTLSProvider
}

// NewGorillaWSLeaderDialer returns a dialer using provider for client mTLS.
func NewGorillaWSLeaderDialer(provider *rafttransport.DynamicTLSProvider) *GorillaWSLeaderDialer {
	return &GorillaWSLeaderDialer{provider: provider}
}

var _ WSLeaderDialer = (*GorillaWSLeaderDialer)(nil)

func (d *GorillaWSLeaderDialer) Dial(ctx context.Context, rawURL, serverName string, headers map[string][]string) (WSConn, error) {
	dialer := &websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		NetDialContext:   (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
	}
	if d.provider != nil {
		dialer.TLSClientConfig = d.provider.DialTLSConfig(serverName)
	} else {
		dialer.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	conn, resp, err := dialer.DialContext(ctx, rawURL, headers)
	if err != nil {
		if resp != nil {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("dial leader websocket: %w (status %s)", err, resp.Status)
		}
		return nil, fmt.Errorf("dial leader websocket: %w", err)
	}
	return conn, nil
}
