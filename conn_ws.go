package webdial

import (
	"bytes"
	"io"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type wsConn struct {
	ws        *websocket.Conn
	reader    io.Reader
	mu        sync.Mutex
	writeMu   sync.Mutex
	done      chan struct{}
	closeOnce sync.Once
}

func newWSConn(ws *websocket.Conn, keepAlive time.Duration) net.Conn {
	c := &wsConn{
		ws:   ws,
		done: make(chan struct{}),
	}
	if keepAlive >= 0 {
		go c.pingLoop(keepAlive)
	}
	return c
}

func (c *wsConn) pingLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
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
		n, err := c.reader.Read(b)
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

func (c *wsConn) Write(b []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	err := c.ws.WriteMessage(websocket.BinaryMessage, b)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

// handleControl responds to application-level text control frames. A "ping:<ts>"
// frame is echoed back as "pong:<ts>" so the peer can measure round-trip latency.
func (c *wsConn) handleControl(payload []byte) {
	if ts, ok := bytes.CutPrefix(payload, []byte("ping:")); ok {
		c.writeControl(append([]byte("pong:"), ts...))
	}
}

func (c *wsConn) writeControl(payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.ws.WriteMessage(websocket.TextMessage, payload)
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
	return c.ws.SetWriteDeadline(t)
}

func (c *wsConn) SetReadDeadline(t time.Time) error {
	return c.ws.SetReadDeadline(t)
}

func (c *wsConn) SetWriteDeadline(t time.Time) error {
	return c.ws.SetWriteDeadline(t)
}
