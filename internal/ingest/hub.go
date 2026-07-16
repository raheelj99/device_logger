// Package ingest turns authenticated MQTT payloads into signed, chained,
// stored log entries, and fans live entries out to gRPC Tail subscribers.
package ingest

import (
	"sync"

	devicelogv1 "devlog/api/gen/devicelog/v1"
)

// Hub is an in-process broadcast of freshly ingested entries. Delivery is
// best-effort: a slow subscriber drops entries rather than stalling ingest —
// history stays authoritative in storage.
type Hub struct {
	mu   sync.RWMutex
	next int
	subs map[int]chan *devicelogv1.LogEntry
}

func NewHub() *Hub {
	return &Hub{subs: map[int]chan *devicelogv1.LogEntry{}}
}

func (h *Hub) Subscribe(buffer int) (int, <-chan *devicelogv1.LogEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.next
	h.next++
	ch := make(chan *devicelogv1.LogEntry, buffer)
	h.subs[id] = ch
	return id, ch
}

func (h *Hub) Unsubscribe(id int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ch, ok := h.subs[id]; ok {
		delete(h.subs, id)
		close(ch)
	}
}

func (h *Hub) Publish(e *devicelogv1.LogEntry) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, ch := range h.subs {
		select {
		case ch <- e:
		default: // subscriber too slow — drop
		}
	}
}
