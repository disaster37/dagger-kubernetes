package telemetry

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type LiveClient struct {
	Conn    *websocket.Conn
	TraceID string
	Send    chan []byte
}

type LiveHub struct {
	mu       sync.RWMutex
	clients  map[string]map[*LiveClient]bool
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
		client.Conn.Close()
	}()

	for {
		select {
		case message, ok := <-client.Send:
			if !ok {
				client.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			client.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := client.Conn.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}
		case <-ticker.C:
			client.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := client.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
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
