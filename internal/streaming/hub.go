package streaming

import (
	"github.com/flagforge/flagforge/pkg/types"
	"sync"
)

// Hub coalesces slow-client notifications in bounded one-element channels.
// Each notification causes a complete resync so intermediate versions are unnecessary.
type Hub struct {
	mu          sync.Mutex
	subscribers map[string]map[chan types.Event]struct{}
}

func New() *Hub { return &Hub{subscribers: map[string]map[chan types.Event]struct{}{}} }
func (h *Hub) Subscribe(envID string) (<-chan types.Event, func()) {
	ch := make(chan types.Event, 1)
	h.mu.Lock()
	if h.subscribers[envID] == nil {
		h.subscribers[envID] = map[chan types.Event]struct{}{}
	}
	h.subscribers[envID][ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subscribers[envID], ch)
			if len(h.subscribers[envID]) == 0 {
				delete(h.subscribers, envID)
			}
			h.mu.Unlock()
		})
	}
}
func (h *Hub) Publish(e types.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subscribers[e.EnvironmentID] {
		select {
		case ch <- e:
		default:
			select {
			case queued := <-ch:
				if queued.Version > e.Version {
					e = queued
				}
			default:
			}
			select {
			case ch <- e:
			default:
			}
		}
	}
}
