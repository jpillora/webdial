// silentserver is a WebSocket or SSE peer that never answers a ping, so the
// client's staleness watchdog is the only thing deciding the connection's
// fate. It is deliberately not a webdial.Server — webdial answers pings itself,
// which is exactly the behaviour under test.
//
// Two modes, selected by path:
//
//	/talking — streams frames continuously while ignoring pings. The client
//	           must keep this connection: a peer sending data is a peer that
//	           is there, whatever happened to the pong.
//	/burst   — sends one frame after the first ping and then becomes silent. The
//	           frame answers that probe, but a later probe must still time out.
//	/silent  — accepts the connection and then says nothing at all. The client
//	           must eventually close this one, or the watchdog is useless.
package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

func drainReads(ws *websocket.Conn, rejectPing bool) {
	for {
		mt, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		if rejectPing && mt == websocket.TextMessage && bytes.HasPrefix(data, []byte("ping:")) {
			ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "unexpected ping"), time.Now().Add(time.Second))
			return
		}
	}
}

func talking(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	go drainReads(ws, r.URL.Query().Get("rejectPing") == "1")
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for range tick.C {
		if err := ws.WriteMessage(websocket.BinaryMessage, []byte("frame")); err != nil {
			return
		}
	}
}

func silent(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	drainReads(ws, false)
}

func burst(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	time.Sleep(150 * time.Millisecond)
	if err := ws.WriteMessage(websocket.BinaryMessage, []byte("frame")); err != nil {
		return
	}
	drainReads(ws, false)
}

func sse(mode string, w http.ResponseWriter, r *http.Request) {
	// SSE client writes and liveness pings arrive as POSTs. Deliberately accept
	// them without sending a pong so only incoming data can satisfy a probe.
	if r.Method == http.MethodPost {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fmt.Fprint(w, "event: sid\ndata: watchdog-test\n\n")
	flusher.Flush()

	sendFrame := func() bool {
		_, err := fmt.Fprint(w, "event: d\ndata: ZnJhbWU=\n\n")
		flusher.Flush()
		return err == nil
	}

	switch mode {
	case "talking":
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				if !sendFrame() {
					return
				}
			case <-r.Context().Done():
				return
			}
		}
	case "burst":
		timer := time.NewTimer(150 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			if !sendFrame() {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
	<-r.Context().Done()
}

func transport(mode string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || r.Header.Get("Accept") == "text/event-stream" {
			sse(mode, w, r)
			return
		}
		switch mode {
		case "talking":
			talking(w, r)
		case "burst":
			burst(w, r)
		default:
			silent(w, r)
		}
	}
}

func main() {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	fmt.Println("http://" + ln.Addr().String())
	mux := http.NewServeMux()
	mux.HandleFunc("/talking", transport("talking"))
	mux.HandleFunc("/burst", transport("burst"))
	mux.HandleFunc("/silent", transport("silent"))
	http.Serve(ln, mux)
}
