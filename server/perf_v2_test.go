package server

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/gorilla/websocket"
)

func TestPerfV2GzipMiddleware(t *testing.T) {
	largeBody := strings.Repeat(`{"hello":"world"}`, 150) // > 1KB
	h := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/large":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, largeBody)
		case "/api/small":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true}`)
		case "/api/logs/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, strings.Repeat("data: ok\n\n", 150))
		}
	}))

	// 1. Large JSON compressed when Accept-Encoding: gzip.
	req := httptest.NewRequest(http.MethodGet, "/api/large", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding=%q, want gzip", got)
	}
	gr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	unzipped, err := io.ReadAll(gr)
	_ = gr.Close()
	if err != nil || string(unzipped) != largeBody {
		t.Fatalf("decoded mismatch (err=%v, len=%d)", err, len(unzipped))
	}

	// 2. Small response < 1KB stays uncompressed.
	reqSmall := httptest.NewRequest(http.MethodGet, "/api/small", nil)
	reqSmall.Header.Set("Accept-Encoding", "gzip")
	recSmall := httptest.NewRecorder()
	h.ServeHTTP(recSmall, reqSmall)
	if got := recSmall.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("small response should skip gzip, got %q", got)
	}

	// 3. SSE endpoint skips gzip.
	reqSSE := httptest.NewRequest(http.MethodGet, "/api/logs/stream", nil)
	reqSSE.Header.Set("Accept-Encoding", "gzip")
	recSSE := httptest.NewRecorder()
	h.ServeHTTP(recSSE, reqSSE)
	if got := recSSE.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("SSE should skip gzip, got %q", got)
	}
}

func TestPerfV2TaskWakeWorkersBroadcastsAllWaiters(t *testing.T) {
	task := &Task{ID: "99", notify: make(chan struct{}, 1)}
	const nWaiters = 3
	woke := make(chan int, nWaiters)
	var wg sync.WaitGroup
	for i := range nWaiters {
		i := i
		ch := task.wakeSignal()
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !waitWorkerWake(context.Background(), ch, 5*time.Second) {
				woke <- i
			}
		}()
	}
	task.WakeWorkers()
	wg.Wait()
	if len(woke) != nWaiters {
		t.Fatalf("woke %d workers, want %d", len(woke), nWaiters)
	}
}

func TestPerfV2DeltaHubAndWSStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	tok, err := signJWT(key)
	if err != nil {
		t.Fatalf("signJWT: %v", err)
	}
	srv := &Server{
		ctx:      ctx,
		m:        &Manager{},
		jwtKey:   key,
		deltaHub: NewDeltaHub(),
		engine:   &Engine{bc: NewBroadcaster()},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/ws", srv.wsStream)
	ts := httptest.NewServer(gzipMiddleware(mux))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/ws?token=" + tok
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteJSON(wsControlMsg{Op: "sub", Channel: "conv:7"}); err != nil {
		t.Fatalf("ws sub: %v", err)
	}

	var ack wsOutbound
	if err := conn.ReadJSON(&ack); err != nil {
		t.Fatalf("read sub ack: %v", err)
	}
	if ack.Event != "subscribed" || ack.Channel != "conv:7" {
		t.Fatalf("unexpected ack: %+v", ack)
	}

	srv.deltaHub.Publish("conv:7", DeltaMsg{
		Event: "delta",
		Data:  DeltaFrame{Worker: "helper", Kind: "text", Text: "hello"},
	})
	_ = srv.engine.Broadcaster()
	_ = db.Activity{}

	var msg wsOutbound
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatalf("read delta msg: %v", err)
	}
	if msg.Channel != "conv:7" || msg.Event != "delta" {
		t.Fatalf("unexpected delta outbound: %+v", msg)
	}
}
