package webdial

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jpillora/eventsource"
)

type sseClientConn struct {
	noopDeadline
	baseURL    string
	sessionID  string
	postURL    string
	sseResp    *http.Response
	decoder    *eventsource.Decoder
	readBuf    bytes.Buffer
	writeMu    sync.Mutex
	client     *http.Client
	closed     atomic.Bool
	localAddr  addr
	remoteAddr addr
}

func newSSEClientConn(baseURL, sessionID string, sseResp *http.Response, decoder *eventsource.Decoder, client *http.Client) *sseClientConn {
	sep := "?"
	if strings.Contains(baseURL, "?") {
		sep = "&"
	}
	return &sseClientConn{
		baseURL:    baseURL,
		sessionID:  sessionID,
		postURL:    baseURL + sep + "s=" + sessionID,
		sseResp:    sseResp,
		decoder:    decoder,
		client:     client,
		localAddr:  addr{transport: "sse", url: "local"},
		remoteAddr: addr{transport: "sse", url: baseURL},
	}
}

func (c *sseClientConn) Read(b []byte) (int, error) {
	for {
		if c.readBuf.Len() > 0 {
			return c.readBuf.Read(b)
		}
		if c.closed.Load() {
			return 0, io.EOF
		}
		var ev eventsource.Event
		if err := c.decoder.Decode(&ev); err != nil {
			return 0, err
		}
		switch ev.Type {
		case "d":
			decoded, err := base64.RawStdEncoding.DecodeString(string(ev.Data))
			if err != nil {
				return 0, fmt.Errorf("webdial: base64 decode: %w", err)
			}
			c.readBuf.Write(decoded)
		case "close":
			c.closed.Store(true)
			return 0, io.EOF
		}
	}
}

func (c *sseClientConn) Write(b []byte) (int, error) {
	if c.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	url := c.postURL
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return 0, fmt.Errorf("webdial: post returned %d", resp.StatusCode)
	}
	return len(b), nil
}

func (c *sseClientConn) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	url := c.postURL + "&close=1"
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, url, nil)
	resp, err := c.client.Do(req)
	if err == nil {
		resp.Body.Close()
	}
	c.sseResp.Body.Close()
	return nil
}

func (c *sseClientConn) LocalAddr() net.Addr  { return c.localAddr }
func (c *sseClientConn) RemoteAddr() net.Addr { return c.remoteAddr }

type sseServerConn struct {
	noopDeadline
	sessionID  string
	w          http.ResponseWriter
	readPipe   *io.PipeReader
	writePipe  *io.PipeWriter
	writeMu    sync.Mutex
	closed     atomic.Bool
	closeCh    chan struct{}
	localAddr  addr
	remoteAddr addr
}

func (c *sseServerConn) Read(b []byte) (int, error) {
	return c.readPipe.Read(b)
}

// writeEvent serializes all access to the SSE response. The session is made
// available to POST handlers before the sid event is flushed, so even the
// initial event must use the same lock as data and control events.
func (c *sseServerConn) writeEvent(ev eventsource.Event) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	// Close marks the connection closed before emitting its final event. No
	// other event may be written after that point.
	if c.closed.Load() && ev.Type != "close" {
		return io.ErrClosedPipe
	}
	return eventsource.WriteEvent(c.w, ev)
}

// Write encodes b and writes it as an SSE event.
// Note: eventsource.WriteEvent also flushes the http.ResponseWriter.
func (c *sseServerConn) Write(b []byte) (int, error) {
	encoded := base64.RawStdEncoding.EncodeToString(b)
	err := c.writeEvent(eventsource.Event{
		Type: "d",
		Data: []byte(encoded),
	})
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *sseServerConn) writeHeartbeat() error {
	return c.writeEvent(eventsource.Event{Type: "ping"})
}

// writePong echoes a client-supplied timestamp back over the SSE stream so the
// client can measure round-trip latency.
func (c *sseServerConn) writePong(ts []byte) error {
	return c.writeEvent(eventsource.Event{Type: "pong", Data: ts})
}

func (c *sseServerConn) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	c.writeEvent(eventsource.Event{Type: "close"})
	c.readPipe.Close()
	close(c.closeCh)
	return nil
}

func (c *sseServerConn) LocalAddr() net.Addr  { return c.localAddr }
func (c *sseServerConn) RemoteAddr() net.Addr { return c.remoteAddr }

type sseSession struct {
	conn *sseServerConn
}
