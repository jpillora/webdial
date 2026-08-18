package webdial

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jpillora/eventsource"
)

type sseClientConn struct {
	baseURL       string
	sessionID     string
	postURL       string
	sseResp       *http.Response
	decoder       *eventsource.Decoder
	readEvents    chan []byte
	readBuf       bytes.Buffer
	readMu        sync.Mutex
	readDeadline  connDeadline
	writeMu       sync.Mutex
	writeDeadline connDeadline
	client        *http.Client
	ctx           context.Context
	cancel        context.CancelFunc
	closed        atomic.Bool
	terminalMu    sync.Mutex
	terminalErr   error
	localAddr     addr
	remoteAddr    addr
}

func newSSEClientConn(baseURL, sessionID string, sseResp *http.Response, decoder *eventsource.Decoder, client *http.Client, ctx context.Context, cancel context.CancelFunc) (*sseClientConn, error) {
	postURL, err := appendURLQueryParam(baseURL, "s", sessionID)
	if err != nil {
		return nil, err
	}
	c := &sseClientConn{
		baseURL:       baseURL,
		sessionID:     sessionID,
		postURL:       postURL,
		sseResp:       sseResp,
		decoder:       decoder,
		readEvents:    make(chan []byte),
		readDeadline:  newConnDeadline(),
		writeDeadline: newConnDeadline(),
		client:        client,
		ctx:           ctx,
		cancel:        cancel,
		localAddr:     addr{transport: "sse", url: "local"},
		remoteAddr:    addr{transport: "sse", url: baseURL},
	}
	go c.decodeEvents()
	return c, nil
}

func (c *sseClientConn) Read(b []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if len(b) == 0 {
		return 0, nil
	}
	for {
		if c.readBuf.Len() > 0 {
			return c.readBuf.Read(b)
		}
		if err := c.readTerminalError(); err != nil {
			return 0, err
		}
		if c.closed.Load() {
			return 0, net.ErrClosed
		}

		deadline := c.readDeadline.snapshot()
		if deadline.expired {
			if c.readDeadline.expired() {
				return 0, os.ErrDeadlineExceeded
			}
			continue
		}
		select {
		case data := <-c.readEvents:
			deadline.stop()
			c.readBuf.Write(data)
		case <-c.ctx.Done():
			deadline.stop()
			if err := c.readTerminalError(); err != nil {
				return 0, err
			}
			return 0, net.ErrClosed
		case <-deadline.timerC():
			if c.readDeadline.expired() {
				return 0, os.ErrDeadlineExceeded
			}
		case <-deadline.changed:
			deadline.stop()
		}
	}
}

func (c *sseClientConn) decodeEvents() {
	for {
		var ev eventsource.Event
		if err := c.decoder.Decode(&ev); err != nil {
			c.setTerminalError(err)
			return
		}
		switch ev.Type {
		case "d":
			decoded, err := base64.RawStdEncoding.DecodeString(string(ev.Data))
			if err != nil {
				c.setTerminalError(fmt.Errorf("webdial: base64 decode: %w", err))
				return
			}
			select {
			case c.readEvents <- decoded:
			case <-c.ctx.Done():
				return
			}
		case "close":
			c.setTerminalError(io.EOF)
			return
		}
	}
}

func (c *sseClientConn) setTerminalError(err error) {
	c.terminalMu.Lock()
	if c.terminalErr == nil {
		c.terminalErr = err
	}
	c.terminalMu.Unlock()
	c.closed.Store(true)
	c.cancel()
	c.sseResp.Body.Close()
}

func (c *sseClientConn) readTerminalError() error {
	c.terminalMu.Lock()
	defer c.terminalMu.Unlock()
	return c.terminalErr
}

func (c *sseClientConn) Write(b []byte) (int, error) {
	if c.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	if c.writeDeadline.expired() {
		return 0, os.ErrDeadlineExceeded
	}

	opCtx, cancel := context.WithCancel(c.ctx)
	req, err := http.NewRequestWithContext(opCtx, http.MethodPost, c.postURL, bytes.NewReader(b))
	if err != nil {
		cancel()
		return 0, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	type postResult struct {
		status int
		err    error
	}
	result := make(chan postResult, 1)
	go func() {
		resp, err := c.client.Do(req)
		if err != nil {
			result <- postResult{err: err}
			return
		}
		resp.Body.Close()
		result <- postResult{status: resp.StatusCode}
	}()

	for {
		deadline := c.writeDeadline.snapshot()
		if deadline.expired {
			if c.writeDeadline.expired() {
				cancel()
				<-result
				return 0, os.ErrDeadlineExceeded
			}
			continue
		}
		select {
		case got := <-result:
			deadline.stop()
			cancel()
			if c.closed.Load() {
				return 0, io.ErrClosedPipe
			}
			if got.err != nil {
				return 0, got.err
			}
			if got.status != http.StatusNoContent {
				return 0, fmt.Errorf("webdial: post returned %d", got.status)
			}
			return len(b), nil
		case <-c.ctx.Done():
			deadline.stop()
			cancel()
			<-result
			return 0, io.ErrClosedPipe
		case <-deadline.timerC():
			if c.writeDeadline.expired() {
				cancel()
				<-result
				return 0, os.ErrDeadlineExceeded
			}
		case <-deadline.changed:
			deadline.stop()
		}
	}
}

func (c *sseClientConn) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	// Release a blocked stream read immediately. The dial context no longer owns
	// this request after establishment; Close is its lifetime boundary. Canceling
	// the stream also tears down the server session, so Close does not wait behind
	// writeMu to send a redundant close POST.
	c.cancel()
	c.sseResp.Body.Close()
	return nil
}

func (c *sseClientConn) LocalAddr() net.Addr  { return c.localAddr }
func (c *sseClientConn) RemoteAddr() net.Addr { return c.remoteAddr }

func (c *sseClientConn) SetDeadline(t time.Time) error {
	if c.closed.Load() {
		return net.ErrClosed
	}
	c.readDeadline.set(t)
	c.writeDeadline.set(t)
	return nil
}

func (c *sseClientConn) SetReadDeadline(t time.Time) error {
	if c.closed.Load() {
		return net.ErrClosed
	}
	c.readDeadline.set(t)
	return nil
}

func (c *sseClientConn) SetWriteDeadline(t time.Time) error {
	if c.closed.Load() {
		return net.ErrClosed
	}
	c.writeDeadline.set(t)
	return nil
}

type sseServerConn struct {
	sessionID     string
	w             http.ResponseWriter
	response      *http.ResponseController
	responseMu    sync.Mutex
	inbound       chan []byte
	readBuf       bytes.Buffer
	readMu        sync.Mutex
	readDeadline  connDeadline
	writeDeadline connDeadline
	writeMu       sync.Mutex
	closed        atomic.Bool
	closeCh       chan struct{}
	localAddr     addr
	remoteAddr    addr
}

func (c *sseServerConn) Read(b []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if len(b) == 0 {
		return 0, nil
	}
	for {
		if c.readBuf.Len() > 0 {
			return c.readBuf.Read(b)
		}
		if c.closed.Load() {
			return 0, net.ErrClosed
		}
		deadline := c.readDeadline.snapshot()
		if deadline.expired {
			if c.readDeadline.expired() {
				return 0, os.ErrDeadlineExceeded
			}
			continue
		}
		select {
		case data := <-c.inbound:
			deadline.stop()
			c.readBuf.Write(data)
		case <-c.closeCh:
			deadline.stop()
			return 0, net.ErrClosed
		case <-deadline.timerC():
			if c.readDeadline.expired() {
				return 0, os.ErrDeadlineExceeded
			}
		case <-deadline.changed:
			deadline.stop()
		}
	}
}

func (c *sseServerConn) deliver(ctx context.Context, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if c.closed.Load() {
		return net.ErrClosed
	}
	select {
	case c.inbound <- data:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closeCh:
		return net.ErrClosed
	}
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
	if c.writeDeadline.expired() {
		return os.ErrDeadlineExceeded
	}
	return eventsource.WriteEvent(c.w, ev)
}

// finishResponse prevents any application or control write from outliving the
// HTTP handler that owns the ResponseWriter. Marking the connection first
// rejects future writes; taking writeMu waits for a write already in flight.
func (c *sseServerConn) finishResponse() {
	c.shutdown()
	c.writeMu.Lock()
	c.writeMu.Unlock()
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
	c.shutdown()
	return nil
}

func (c *sseServerConn) shutdown() {
	if !c.closed.CompareAndSwap(false, true) {
		return
	}
	// Force an in-flight ResponseWriter call to return before the owning HTTP
	// handler finishes. Ignore ErrNotSupported here; SetWriteDeadline reports it
	// to callers, while shutdown must remain idempotent and best effort.
	c.responseMu.Lock()
	_ = c.response.SetWriteDeadline(time.Now())
	c.responseMu.Unlock()
	close(c.closeCh)
}

func (c *sseServerConn) LocalAddr() net.Addr  { return c.localAddr }
func (c *sseServerConn) RemoteAddr() net.Addr { return c.remoteAddr }

func (c *sseServerConn) SetDeadline(t time.Time) error {
	c.responseMu.Lock()
	defer c.responseMu.Unlock()
	if c.closed.Load() {
		return net.ErrClosed
	}
	if err := c.response.SetWriteDeadline(t); err != nil {
		return err
	}
	c.readDeadline.set(t)
	c.writeDeadline.set(t)
	return nil
}

func (c *sseServerConn) SetReadDeadline(t time.Time) error {
	if c.closed.Load() {
		return net.ErrClosed
	}
	c.readDeadline.set(t)
	return nil
}

func (c *sseServerConn) SetWriteDeadline(t time.Time) error {
	c.responseMu.Lock()
	defer c.responseMu.Unlock()
	// The lifecycle check and ResponseController call are one critical section:
	// finishResponse may return as soon as shutdown marks the connection closed.
	// No deadline call may touch the ResponseWriter after that point.
	if c.closed.Load() {
		return net.ErrClosed
	}
	if err := c.response.SetWriteDeadline(t); err != nil {
		return err
	}
	c.writeDeadline.set(t)
	return nil
}

type sseSession struct {
	conn     *sseServerConn
	postLock chan struct{}
}

func newSSESession(conn *sseServerConn) *sseSession {
	return &sseSession{
		conn:     conn,
		postLock: make(chan struct{}, 1),
	}
}

func (s *sseSession) acquirePost(ctx context.Context) error {
	if s.conn.closed.Load() {
		return io.ErrClosedPipe
	}
	select {
	case s.postLock <- struct{}{}:
		if s.conn.closed.Load() {
			s.releasePost()
			return io.ErrClosedPipe
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.conn.closeCh:
		return io.ErrClosedPipe
	}
}

func (s *sseSession) releasePost() {
	<-s.postLock
}
