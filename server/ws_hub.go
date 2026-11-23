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

	enqueue := func(msg wsOutbound) {
		select {
		case outCh <- msg:
		case <-doneCh:
		default:
			// Backpressure: drop ephemeral delta/log frame when socket stalls.
		}
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

	// Single writer goroutine (serializes control/ping/data writes).
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer shutdown()
		ping := time.NewTicker(25 * time.Second)
		defer ping.Stop()
		lastAuth := time.Now()
		for {
			select {
			case <-doneCh:
				return
			case <-s.ctx.Done():
				return
			case msg := <-outCh:
				if time.Since(lastAuth) >= 25*time.Second {
					if !s.sessionOK(tok) {
						return
					}
					lastAuth = time.Now()
				}
				_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if err := conn.WriteJSON(msg); err != nil {
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
