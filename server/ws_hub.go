package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type wsControlMsg struct {
	Op      string `json:"op"`                // "sub" | "unsub" | "ping"
	Channel string `json:"channel,omitempty"` // "task:<id>" | "conv:<id>" | "side:<id>" | "logs"
	Since   int64  `json:"since,omitempty"`
}

type wsOutbound struct {
	Channel string `json:"channel,omitempty"`
	Event   string `json:"event"` // "activity" | "delta" | "side" | "log" | "status" | "subscribed" | "pong"
	Data    any    `json:"data,omitempty"`
}

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:    4096,
	WriteBufferSize:   16384,
	EnableCompression: true,
	CheckOrigin: func(r *http.Request) bool {
		orig := strings.TrimSpace(r.Header.Get("Origin"))
		if orig == "" {
			return true
		}
		u, err := url.Parse(orig)
		if err != nil {
			return false
		}
		if strings.EqualFold(u.Host, r.Host) {
			return true
		}
		h := strings.ToLower(u.Hostname())
		return h == "localhost" || h == "127.0.0.1" || h == "::1"
	},
}

// wsStream provides a single multiplexed WebSocket endpoint (/api/ws) carrying
// live task activities/deltas, conversation deltas/activities, side-question
// snapshots, and server logs without saturating HTTP/1.1 browser connection limits.
func (s *Server) wsStream(w http.ResponseWriter, r *http.Request) {
	tok := extractToken(r)
	if !s.sessionOK(tok) {
		writeErr(w, 401, "token is invalid or expired")
		return
	}
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(64 * 1024)

	outCh := make(chan wsOutbound, 512)
	doneCh := make(chan struct{})
	var closeOnce sync.Once
	shutdown := func() { closeOnce.Do(func() { close(doneCh) }) }
	defer shutdown()

	// Lossless coalescing queue for high-frequency token deltas: under backpressure
	// (slow socket or multi-worker bursts), consecutive deltas for the same
	// (channel, worker, intent_id, kind) are concatenated instead of dropped.
	var deltaMu sync.Mutex
	deltaOrder := make([]string, 0, 8)
	deltaMap := make(map[string]wsOutbound, 8)
	resyncMap := make(map[string]struct{})
	deltaNotify := make(chan struct{}, 1)
	pingDelta := func() {
		select {
		case deltaNotify <- struct{}{}:
		default:
		}
	}

	deltaKey := func(ch string, df DeltaFrame) string {
		var iid string
		if df.IntentID != nil {
			iid = strconv.FormatInt(*df.IntentID, 10)
		}
		return ch + "\x00" + df.Worker + "\x00" + iid + "\x00" + df.Kind
	}

	enqueue := func(msg wsOutbound) {
		if msg.Event == "delta" {
			if df, ok := msg.Data.(DeltaFrame); ok {
				k := deltaKey(msg.Channel, df)
				deltaMu.Lock()
				if cur, exists := deltaMap[k]; exists && !df.Reset {
					prev := cur.Data.(DeltaFrame)
					prev.Text += df.Text
					cur.Data = prev
					deltaMap[k] = cur
				} else {
					if !exists {
						deltaOrder = append(deltaOrder, k)
					}
					deltaMap[k] = msg
				}
				deltaMu.Unlock()
				pingDelta()
				return
			}
		}
		select {
		case outCh <- msg:
		case <-doneCh:
		default:
			// Backpressure on reliable channel: mark channel for client resync so
			// activity rows are never silently lost.
			if msg.Channel != "" && (msg.Event == "activity" || msg.Event == "status") {
				deltaMu.Lock()
				resyncMap[msg.Channel] = struct{}{}
				deltaMu.Unlock()
				pingDelta()
			}
		}
	}

	drainCoalesced := func(dst []wsOutbound) []wsOutbound {
		deltaMu.Lock()
		for _, k := range deltaOrder {
			if m, ok := deltaMap[k]; ok {
				dst = append(dst, m)
			}
		}
		deltaOrder = deltaOrder[:0]
		clear(deltaMap)
		for ch := range resyncMap {
			dst = append(dst, wsOutbound{Channel: ch, Event: "resync"})
			delete(resyncMap, ch)
		}
		deltaMu.Unlock()
		return dst
	}

	var subsMu sync.Mutex
	subs := map[string]func(){}
	unsubChannel := func(chName string) {
		subsMu.Lock()
		if fn, ok := subs[chName]; ok {
			delete(subs, chName)
			subsMu.Unlock()
			fn()
			return
		}
		subsMu.Unlock()
	}
	defer func() {
		subsMu.Lock()
		fns := make([]func(), 0, len(subs))
		for k, fn := range subs {
			fns = append(fns, fn)
			delete(subs, k)
		}
		subsMu.Unlock()
		for _, fn := range fns {
			fn()
		}
	}()

	subscribeChannel := func(chName string, since int64) {
		unsubChannel(chName)
		subStop := make(chan struct{})
		var subStopOnce sync.Once
		var wg sync.WaitGroup

		switch {
		case chName == "global":
			dch, dunsub := s.deltaHub.Subscribe("global")
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer dunsub()
				for {
					select {
					case <-doneCh:
						return
					case <-subStop:
						return
					case msg, ok := <-dch:
						if !ok {
							return
						}
						enqueue(wsOutbound{Channel: chName, Event: msg.Event, Data: msg.Data})
					}
				}
			}()
		case chName == "logs":
			lch, lunsub := logSink.subscribe()
			if recent, cursor := logSink.recent(since, 1000); len(recent) > 0 {
				for _, l := range recent {
					enqueue(wsOutbound{Channel: chName, Event: "log", Data: l})
				}
				if cursor > since {
					since = cursor
				}
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer lunsub()
				for {
					select {
					case <-doneCh:
						return
					case <-subStop:
						return
					case l, ok := <-lch:
						if !ok {
							return
						}
						if l.Seq <= since {
							continue
						}
						since = l.Seq
						enqueue(wsOutbound{Channel: chName, Event: "log", Data: l})
					}
				}
			}()
		case strings.HasPrefix(chName, "task:"):
			taskID := strings.TrimPrefix(chName, "task:")
			if t := s.m.ResolveTask(taskID); t != nil {
				ach, aunsub := s.engine.Broadcaster().Subscribe(t.ID)
				dch, dunsub := s.deltaHub.Subscribe(chName)
				lastID := since
				if since > 0 {
					if items, cursor, err := t.Store.ActivityList(nil, since, 200); err == nil {
						for _, a := range items {
							enqueue(wsOutbound{Channel: chName, Event: "activity", Data: activityDTO(a)})
						}
						if cursor > lastID {
							lastID = cursor
						}
					}
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer aunsub()
					defer dunsub()
					for {
						select {
						case <-doneCh:
							return
						case <-subStop:
							return
						case a, ok := <-ach:
							if !ok {
								return
							}
							if a.ID <= lastID {
								continue
							}
							lastID = a.ID
							enqueue(wsOutbound{Channel: chName, Event: "activity", Data: activityDTO(a)})
						case msg, ok := <-dch:
							if !ok {
								return
							}
							enqueue(wsOutbound{Channel: chName, Event: msg.Event, Data: msg.Data})
						}
					}
				}()
			}
		case strings.HasPrefix(chName, "conv:") || strings.HasPrefix(chName, "side:"):
			dch, dunsub := s.deltaHub.Subscribe(chName)
			if strings.HasPrefix(chName, "conv:") && since > 0 && s.m.pg != nil {
				if cid, err := strconv.ParseInt(strings.TrimPrefix(chName, "conv:"), 10, 64); err == nil {
					if items, cursor, err := s.m.pg.ConvActivityList(cid, since, 200); err == nil {
						for _, a := range items {
							enqueue(wsOutbound{Channel: chName, Event: "activity", Data: activityDTO(a)})
						}
						since = cursor
					}
				}
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer dunsub()
				for {
					select {
					case <-doneCh:
						return
					case <-subStop:
						return
					case msg, ok := <-dch:
						if !ok {
							return
						}
						enqueue(wsOutbound{Channel: chName, Event: msg.Event, Data: msg.Data})
					}
				}
			}()
		default:
			return
		}

		subsMu.Lock()
		subs[chName] = func() {
			subStopOnce.Do(func() { close(subStop) })
			wg.Wait()
		}
		subsMu.Unlock()
		enqueue(wsOutbound{Channel: chName, Event: "subscribed"})
	}

	// Single writer goroutine: drains reliable + coalesced delta queues into
	// batched WebSocket frames (up to 64 messages per write) to cut syscalls and
	// permessage-deflate flushes under multi-worker streaming load.
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer shutdown()
		ping := time.NewTicker(25 * time.Second)
		defer ping.Stop()
		lastAuth := time.Now()
		batch := make([]wsOutbound, 0, 64)
		flushBatch := func() bool {
			if len(batch) == 0 {
				return true
			}
			if time.Since(lastAuth) >= 25*time.Second {
				if !s.sessionOK(tok) {
					return false
				}
				lastAuth = time.Now()
			}
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			var err error
			if len(batch) == 1 {
				err = conn.WriteJSON(batch[0])
			} else {
				err = conn.WriteJSON(wsOutbound{Event: "batch", Data: batch})
			}
			batch = batch[:0]
			return err == nil
		}
		drainOutCh := func() {
			for len(batch) < 64 {
				select {
				case m := <-outCh:
					batch = append(batch, m)
				default:
					return
				}
			}
		}
		for {
			select {
			case <-doneCh:
				return
			case <-s.ctx.Done():
				return
			case msg := <-outCh:
				// Flush any coalesced deltas BEFORE appending an outCh activity/status
				// frame so the UI never processes a stale delta after the activity
				// that finalized and cleared it.
				batch = drainCoalesced(batch)
				batch = append(batch, msg)
				drainOutCh()
				if !flushBatch() {
					return
				}
			case <-deltaNotify:
				batch = drainCoalesced(batch)
				drainOutCh()
				if !flushBatch() {
					return
				}
			case <-ping.C:
				if !s.sessionOK(tok) {
					return
				}
				lastAuth = time.Now()
				_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					return
				}
			}
		}
	}()

	_ = conn.SetReadDeadline(time.Now().Add(70 * time.Second))
	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(70 * time.Second))
		return nil
	})
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			break
		}
		_ = conn.SetReadDeadline(time.Now().Add(70 * time.Second))
		var ctrl wsControlMsg
		if json.Unmarshal(raw, &ctrl) != nil {
			continue
		}
		switch ctrl.Op {
		case "sub":
			if ctrl.Channel != "" {
				subscribeChannel(ctrl.Channel, ctrl.Since)
			}
		case "unsub":
			if ctrl.Channel != "" {
				unsubChannel(ctrl.Channel)
			}
		case "ping":
			enqueue(wsOutbound{Event: "pong"})
		}
	}
	shutdown()
	<-writerDone
}
