package server

import "sync"

// DeltaMsg is an ephemeral live-streaming payload pushed through DeltaHub and
// ws_hub / SSE streams without blocking on Postgres writes.
type DeltaMsg struct {
	Event string `json:"event"` // "delta" | "activity" | "side" | "log"
	Data  any    `json:"data"`
}

// DeltaFrame carries a coalesced token/thinking fragment for task or chat UIs.
type DeltaFrame struct {
	Worker   string `json:"worker,omitempty"`
	IntentID *int64 `json:"intent_id,omitempty"`
	Kind     string `json:"kind"` // "text" | "thinking"
	Text     string `json:"text,omitempty"`
	Reset    bool   `json:"reset,omitempty"`
}

// DeltaHub provides non-blocking, channel-keyed in-process pub/sub for live
// ephemeral events ("task:<id>", "conv:<id>", "side:<id>", "logs").
type DeltaHub struct {
	mu   sync.RWMutex
	subs map[string]map[chan DeltaMsg]chan struct{}
}

func NewDeltaHub() *DeltaHub {
	return &DeltaHub{subs: map[string]map[chan DeltaMsg]chan struct{}{}}
}

func (h *DeltaHub) Subscribe(channel string) (<-chan DeltaMsg, func()) {
	ch, _, unsub := h.SubscribeResync(channel)
	return ch, unsub
}

func (h *DeltaHub) SubscribeResync(channel string) (<-chan DeltaMsg, <-chan struct{}, func()) {
	ch := make(chan DeltaMsg, 256)
	ov := make(chan struct{}, 1)
	h.mu.Lock()
	if h.subs[channel] == nil {
		h.subs[channel] = map[chan DeltaMsg]chan struct{}{}
	}
	h.subs[channel][ch] = ov
	h.mu.Unlock()

	var once sync.Once
	return ch, ov, func() {
		once.Do(func() {
			h.mu.Lock()
			if m := h.subs[channel]; m != nil {
				delete(m, ch)
				if len(m) == 0 {
					delete(h.subs, channel)
				}
			}
			h.mu.Unlock()
			close(ch)
		})
	}
}

func (h *DeltaHub) Publish(channel string, msg DeltaMsg) {
	if h == nil {
		return
	}
	h.mu.RLock()
	targets := h.subs[channel]
	if len(targets) == 0 {
		h.mu.RUnlock()
		return
	}
	for ch, ov := range targets {
		select {
		case ch <- msg:
		default:
			select {
			case ov <- struct{}{}:
			default:
			}
		}
	}
	h.mu.RUnlock()
}
