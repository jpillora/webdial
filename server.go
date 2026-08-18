package webdial

import (
	"compress/flate"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jpillora/eventsource"
)

type Server struct {
	// KeepAlive is the interval between keep-alive pings.
	// Zero means 25 seconds. Negative means disabled.
	KeepAlive time.Duration
	// CheckOrigin, when non-nil, validates the Origin header of WebSocket
	// upgrade requests. A nil function uses Gorilla WebSocket's secure default:
	// requests without an Origin are accepted, while requests with an Origin
	// must have an Origin host matching the request Host. CheckOrigin may be
	// called concurrently and must be safe for concurrent use.
	CheckOrigin func(*http.Request) bool
	// CompressionLevel is the flate level for per-message WS compression
	// (1-9, or flate.DefaultCompression). Zero means flate.BestSpeed.
	CompressionLevel int
	// WriteTimeout bounds a single WS frame write, so a peer that stops
	// reading cannot pin the connection forever. Zero means 30 seconds.
	// Negative means unbounded. Ignored once the caller sets its own write
	// deadline via the net.Conn interface.
	WriteTimeout time.Duration
	acceptCh     chan net.Conn
	sessions     sync.Map // map[string]*sseSession
	closed       chan struct{}
	closeOnce    sync.Once
}

func NewServer() *Server {
	return &Server{
		acceptCh: make(chan net.Conn, 16),
		closed:   make(chan struct{}),
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) keepAliveInterval() time.Duration {
	if s.KeepAlive == 0 {
		return 25 * time.Second
	}
	return s.KeepAlive
}

func (s *Server) compressionLevel() int {
	if s.CompressionLevel == 0 {
		return flate.BestSpeed
	}
	return s.CompressionLevel
}

func (s *Server) writeTimeout() time.Duration {
	if s.WriteTimeout == 0 {
		return 30 * time.Second
	}
	if s.WriteTimeout < 0 {
		return 0
	}
	return s.WriteTimeout
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	// The upgrader is request-local so different Server instances can safely
	// use different origin policies. Leaving CheckOrigin nil deliberately uses
	// Gorilla's same-origin default.
	upgrader := websocket.Upgrader{
		CheckOrigin:       s.CheckOrigin,
		EnableCompression: true, // per-message deflate (RFC 7692); browser decompresses natively
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn := newWSConn(ws, s.keepAliveInterval(), s.compressionLevel(), s.writeTimeout())
	select {
	case s.acceptCh <- conn:
	case <-s.closed:
		conn.Close()
	}
}

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	sid := generateSessionID()
	conn := &sseServerConn{
		sessionID:     sid,
		w:             w,
		response:      http.NewResponseController(w),
		inbound:       make(chan []byte),
		readDeadline:  newConnDeadline(),
		writeDeadline: newConnDeadline(),
		closeCh:       make(chan struct{}),
		localAddr:     addr{transport: "sse", url: "server"},
		remoteAddr:    addr{transport: "sse", url: r.RemoteAddr},
	}
	s.sessions.Store(sid, &sseSession{conn: conn})
	defer func() {
		conn.finishResponse()
		s.sessions.Delete(sid)
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	if err := conn.writeEvent(eventsource.Event{
		Type: "sid",
		Data: []byte(sid),
	}); err != nil {
		conn.Close()
		return
	}
	select {
	case s.acceptCh <- conn:
	case <-s.closed:
		conn.Close()
		return
	}
	ka := s.keepAliveInterval()
	if ka < 0 {
		select {
		case <-r.Context().Done():
		case <-conn.closeCh:
		case <-s.closed:
		}
		return
	}
	ticker := time.NewTicker(ka)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := conn.writeHeartbeat(); err != nil {
				return
			}
		case <-r.Context().Done():
			return
		case <-conn.closeCh:
			return
		case <-s.closed:
			return
		}
	}
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
	if ping := r.URL.Query().Get("ping"); ping != "" {
		if err := sess.conn.writePong([]byte(ping)); err != nil {
			http.Error(w, "connection write failed", http.StatusGone)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}
	if err := sess.conn.deliver(r.Context(), body); err != nil {
		http.Error(w, "connection delivery failed", http.StatusGone)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
