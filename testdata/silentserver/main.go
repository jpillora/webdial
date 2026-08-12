// silentserver is a WebSocket peer that never answers a ping, so the client's
// staleness watchdog is the only thing deciding the connection's fate. It is
// deliberately not a webdial.Server — webdial answers pings itself, which is
// exactly the behaviour under test.
//
// Two modes, selected by path:
//
//	/talking — streams frames continuously while ignoring pings. The client
//	           must keep this connection: a peer sending data is a peer that
//	           is there, whatever happened to the pong.
//	/silent  — accepts the connection and then says nothing at all. The client
//	           must eventually close this one, or the watchdog is useless.
package main

import (
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

func drainReads(ws *websocket.Conn) {
	for {
		if _, _, err := ws.ReadMessage(); err != nil {
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
	go drainReads(ws)
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
	drainReads(ws)
}

func main() {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	fmt.Println("http://" + ln.Addr().String())
	mux := http.NewServeMux()
	mux.HandleFunc("/talking", talking)
	mux.HandleFunc("/silent", silent)
	http.Serve(ln, mux)
}
