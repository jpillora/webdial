package webdial

import (
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/jpillora/eventsource"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type Server struct {
	acceptCh  chan net.Conn
	sessions  sync.Map // map[string]*sseSession
	closed    chan struct{}
	closeOnce sync.Once
}

func NewServer() *Server {
	return &Server{
		acceptCh: make(chan net.Conn, 16),
		closed:   make(chan struct{}),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", s.handleWS)
	mux.HandleFunc("GET /sse", s.handleSSE)
	mux.HandleFunc("POST /post", s.handlePost)
	mux.HandleFunc("/", s.handleRoot)
	return mux
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Upgrade") != "" {
		s.handleWS(w, r)
		return
	}
	if r.Method == http.MethodPost {
		s.handlePost(w, r)
		return
	}
	if r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		s.handleSSE(w, r)
		return
	}
	http.Error(w, "webdial: unsupported request", http.StatusBadRequest)
}

func (s *Server) Accept() (net.Conn, error) {
	select {
	case conn := <-s.acceptCh:
		return conn, nil
	case <-s.closed:
		return nil, errors.New("webdial: server closed")
	}
}

func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)
		s.sessions.Range(func(key, value any) bool {
			sess := value.(*sseSession)
			sess.conn.Close()
			s.sessions.Delete(key)
			return true
		})
	})
	return nil
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn := newWSConn(ws)
	select {
	case s.acceptCh <- conn:
	case <-s.closed:
		conn.Close()
	}
}

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	sid := generateSessionID()
	pr, pw := io.Pipe()
	conn := &sseServerConn{
		sessionID:  sid,
		w:          w,
		readPipe:   pr,
		writePipe:  pw,
		closeCh:    make(chan struct{}),
		localAddr:  addr{transport: "sse", url: "server"},
		remoteAddr: addr{transport: "sse", url: r.RemoteAddr},
	}
	s.sessions.Store(sid, &sseSession{conn: conn})
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	eventsource.WriteEvent(w, eventsource.Event{
		Type: "sid",
		Data: []byte(sid),
	})
	select {
	case s.acceptCh <- conn:
	case <-s.closed:
		conn.Close()
		s.sessions.Delete(sid)
		return
	}
	select {
	case <-r.Context().Done():
	case <-conn.closeCh:
	case <-s.closed:
	}
	s.sessions.Delete(sid)
	pw.Close()
}

func (s *Server) handlePost(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("s")
	if sid == "" {
		http.Error(w, "missing session id", http.StatusBadRequest)
		return
	}
	val, ok := s.sessions.Load(sid)
	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	sess := val.(*sseSession)
	if r.URL.Query().Get("close") == "1" {
		sess.conn.Close()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}
	sess.conn.writePipe.Write(body)
	w.WriteHeader(http.StatusNoContent)
}
