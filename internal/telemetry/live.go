package telemetry

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/sse"
)

// LiveClient is a single SSE subscriber for a trace. The SSE writer is owned by
// the HTTP handler that created it; writePump pushes events to it and signals
// done when it exits so the handler can finalize the response.
type LiveClient struct {
	writer  *sse.Writer
	TraceID string
	Send    chan []byte
	done    chan struct{}
}

type LiveHub struct {
	mu      sync.RWMutex
	clients map[string]map[*LiveClient]bool
}

func NewLiveHub() *LiveHub {
	return &LiveHub{
		clients: make(map[string]map[*LiveClient]bool),
	}
}

func (h *LiveHub) Subscribe(traceID string, client *LiveClient) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.clients[traceID] == nil {
		h.clients[traceID] = make(map[*LiveClient]bool)
	}
	h.clients[traceID][client] = true

	go h.writePump(client)
}

func (h *LiveHub) Unsubscribe(traceID string, client *LiveClient) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if clients, ok := h.clients[traceID]; ok {
		if _, ok := clients[client]; ok {
			delete(clients, client)
			close(client.Send)
		}
		if len(clients) == 0 {
			delete(h.clients, traceID)
		}
	}
}

func (h *LiveHub) BroadcastSpanUpdate(traceID string, update interface{}) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	clients, ok := h.clients[traceID]
	if !ok {
		return
	}

	data, err := json.Marshal(update)
	if err != nil {
		return
	}

	for client := range clients {
		select {
		case client.Send <- data:
		default:
		}
	}
}

func (h *LiveHub) writePump(client *LiveClient) {
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		_ = client.writer.Close()
		close(client.done)
	}()

	for {
		select {
		case message, ok := <-client.Send:
			if !ok {
				return
			}
			if err := client.writer.WriteEvent("", "message", message); err != nil {
				return
			}
		case <-ticker.C:
			if err := client.writer.WriteKeepAlive(); err != nil {
				return
			}
		}
	}
}

func (h *LiveHub) ClientCount(traceID string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients[traceID])
}

// NewLiveClient constructs a LiveClient bound to a fresh SSE writer for the
// given request context. The HTTP handler must have set the SSE response
// headers beforehand. Done is closed when writePump exits so the handler can
// finalize the response cleanly.
func NewLiveClient(c *app.RequestContext, traceID string) *LiveClient {
	return &LiveClient{
		writer:  sse.NewWriter(c),
		TraceID: traceID,
		Send:    make(chan []byte, 256),
		done:    make(chan struct{}),
	}
}

// Done returns a channel that is closed when the client's writePump exits.
func (c *LiveClient) Done() <-chan struct{} {
	return c.done
}
