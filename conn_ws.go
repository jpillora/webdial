package webdial

import (
	"bytes"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// pongQueue is deliberately shallow: a peer that has not drained one pong is
// not going to benefit from us buffering more.
const pongQueue = 4

type wsConn struct {
	ws           *websocket.Conn
	reader       io.Reader
	mu           sync.Mutex
	writeMu      sync.Mutex
	writeTimeout time.Duration
	userDeadline atomic.Bool
	pongs        chan []byte
	done         chan struct{}
	closeOnce    sync.Once
}

func newWSConn(ws *websocket.Conn, keepAlive time.Duration, compressionLevel int, writeTimeout time.Duration) net.Conn {
	ws.EnableWriteCompression(true)
	_ = ws.SetCompressionLevel(compressionLevel)
	c := &wsConn{
		ws:           ws,
		writeTimeout: writeTimeout,
		pongs:        make(chan []byte, pongQueue),
		done:         make(chan struct{}),
	}
	go c.controlLoop(keepAlive)
	return c
}

// controlLoop owns every write the read path would otherwise have to make.
// Answering a ping inline would mean taking writeMu while holding the read
// mutex, so a single slow data write would stall all inbound data behind the
// peer's own heartbeat.
func (c *wsConn) controlLoop(keepAlive time.Duration) {
	var ticks <-chan time.Time
	if keepAlive > 0 {
		ticker := time.NewTicker(keepAlive)
		defer ticker.Stop()
		ticks = ticker.C
	}
	for {
		select {
		case <-ticks:
			if err := c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				return
			}
		case payload := <-c.pongs:
			if err := c.writeControl(payload); err != nil {
				return
			}
		case <-c.done:
			return
		}
	}
}

func (c *wsConn) Read(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		if c.reader == nil {
			mt, r, err := c.ws.NextReader()
			if err != nil {
				return 0, err
			}
			if mt == websocket.TextMessage {
				ctrl, _ := io.ReadAll(r)
				c.handleControl(ctrl)
				continue
			}
			c.reader = r
		}
		n, err := c.fill(b)
		if err == io.EOF {
			c.reader = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

// fill reads as much of the current message as b holds.
//
// Gorilla's message reader is a stream: one Read returns only what its internal
// buffer (4096 bytes by default) currently holds, so a larger message arrives
// split across several Reads. Every consumer that parses one frame per Read —
// a length-prefixed frame, a JSON object — then sees a truncated head and a
// stray tail, and typically discards both. Draining the message into the
// caller's buffer makes a Read yield a whole message whenever it fits.
//
// A message larger than b still spans Reads, exactly as before, so consumers
// treating the conn as a byte stream are unaffected. Consumers that need whole
// messages must therefore pass a buffer at least as large as their maximum
// frame.
func (c *wsConn) fill(b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := c.reader.Read(b[n:])
		n += m
		if err != nil {
			return n, err
		}
		if m == 0 {
			break
		}
	}
	return n, nil
}

func (c *wsConn) Write(b []byte) (int, error) {
	if err := c.writeMessage(websocket.BinaryMessage, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

// handleControl responds to application-level text control frames. A "ping:<ts>"
// frame is echoed back as "pong:<ts>" so the peer can measure round-trip latency.
// The reply is handed to controlLoop rather than written here — see its comment.
func (c *wsConn) handleControl(payload []byte) {
	ts, ok := bytes.CutPrefix(payload, []byte("ping:"))
	if !ok {
		return
	}
	select {
	case c.pongs <- append([]byte("pong:"), ts...):
	default:
	}
}

func (c *wsConn) writeControl(payload []byte) error {
	return c.writeMessage(websocket.TextMessage, payload)
}

// writeMessage serialises writes and bounds them. gorilla blocks until the
// frame is flushed, so without a deadline a peer that stops reading pins both
// the connection and writeMu forever.
func (c *wsConn) writeMessage(msgType int, b []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.writeTimeout > 0 && !c.userDeadline.Load() {
		if err := c.ws.SetWriteDeadline(time.Now().Add(c.writeTimeout)); err != nil {
			return err
		}
	}
	return c.ws.WriteMessage(msgType, b)
}

func (c *wsConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.done)
	})
	return c.ws.Close()
}

func (c *wsConn) LocalAddr() net.Addr  { return c.ws.LocalAddr() }
func (c *wsConn) RemoteAddr() net.Addr { return c.ws.RemoteAddr() }

func (c *wsConn) SetDeadline(t time.Time) error {
	if err := c.ws.SetReadDeadline(t); err != nil {
		return err
	}
	return c.SetWriteDeadline(t)
}

func (c *wsConn) SetReadDeadline(t time.Time) error {
	return c.ws.SetReadDeadline(t)
}

func (c *wsConn) SetWriteDeadline(t time.Time) error {
	c.userDeadline.Store(!t.IsZero())
	return c.ws.SetWriteDeadline(t)
}
