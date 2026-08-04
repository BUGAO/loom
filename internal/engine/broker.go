package engine

import "sync"

// Broker fans out run snapshots to SSE subscribers, keyed by run id.
// (The orchestration hub of dynamic mode is internal/hub; this one only
// moves bytes to browsers.)
type Broker struct {
	mu   sync.Mutex
	subs map[string]map[chan []byte]bool
}

func NewBroker() *Broker {
	return &Broker{subs: map[string]map[chan []byte]bool{}}
}

func (h *Broker) Subscribe(runID string) chan []byte {
	ch := make(chan []byte, 16)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[runID] == nil {
		h.subs[runID] = map[chan []byte]bool{}
	}
	h.subs[runID][ch] = true
	return ch
}

func (h *Broker) Unsubscribe(runID string, ch chan []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs[runID], ch)
}

func (h *Broker) Publish(runID string, data []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[runID] {
		select {
		case ch <- data:
		default: // slow subscriber: drop; they resync on next event
		}
	}
}
