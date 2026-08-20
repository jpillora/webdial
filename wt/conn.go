package wt

import (
	"bytes"
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/webtransport-go"
)

var _ net.Conn = (*wtConn)(nil)

// wtConn adapts one WebTransport bidirectional stream to net.Conn. The stream
// carries application bytes only; control messages travel as datagrams, so
// answering a ping never contends with a data write.
type wtConn struct {
	sess   *webtransport.Session
	stream *webtransport.Stream

	closed    atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

func newWTConn(sess *webtransport.Session, stream *webtransport.Stream) net.Conn {
	c := &wtConn{sess: sess, stream: stream}
	go c.controlLoop()
	return c
}

func (c *wtConn) Read(b []byte) (int, error) {
	n, err := c.stream.Read(b)
	return n, c.mapErr(err)
}

func (c *wtConn) Write(b []byte) (int, error) {
	n, err := c.stream.Write(b)
	return n, c.mapErr(err)
}

// controlLoop answers the peer's latency probes. Datagrams are unreliable, so
// a dropped ping or pong costs one latency sample and nothing else; the
// watchdog on the far side only acts after several consecutive misses.
//
// The loop needs no stop channel: Close tears down the session, which cancels
// its context and unblocks ReceiveDatagram.
func (c *wtConn) controlLoop() {
	for {
		msg, err := c.sess.ReceiveDatagram(c.sess.Context())
		if err != nil {
			return
		}
		ts, ok := bytes.CutPrefix(msg, []byte("ping:"))
		if !ok {
			continue
		}
		_ = c.sess.SendDatagram(append([]byte("pong:"), ts...))
	}
}

// mapErr reports a local Close as net.ErrClosed, the way a real net.Conn does,
// without swallowing deadline expiry: a deadline error is non-fatal and the
// stream stays usable afterwards, so it must reach the caller intact.
//
// The test is errors.Is against os.ErrDeadlineExceeded, never net.Error's
// Timeout. A QUIC idle timeout also reports Timeout, and that one is terminal.
func (c *wtConn) mapErr(err error) error {
	if err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		return err
	}
	if c.closed.Load() {
		return net.ErrClosed
	}
	return err
}

// Close half-closes the stream and returns without waiting for the session to
// wind down.
//
// Closing the session is what actually frees the QUIC connection, but it also
// aborts outgoing streams, so doing it here would discard whatever the last
// Write left in the send queue. Since write-then-close is the ordinary way to
// finish a net.Conn, and both other webdial transports deliver that tail, the
// session teardown is deferred to a linger goroutine.
func (c *wtConn) Close() error {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		// The FIN goes out behind any data still queued. CancelRead only
		// concerns our own receive side, so it releases a parked Read without
		// disturbing delivery of what we already wrote.
		c.closeErr = c.stream.Close()
		c.stream.CancelRead(0)
		go c.linger()
	})
	return c.closeErr
}

// linger gives queued stream data a chance to reach the peer before the
// session, and with it the QUIC connection, goes away. A peer that closes its
// own end ends the wait immediately; the timeout is the backstop for one that
// never does.
func (c *wtConn) linger() {
	timer := time.NewTimer(closeLingerTimeout)
	defer timer.Stop()
	select {
	case <-c.sess.Context().Done():
	case <-timer.C:
	}
	c.sess.CloseWithError(0, "")
}

func (c *wtConn) LocalAddr() net.Addr  { return c.sess.LocalAddr() }
func (c *wtConn) RemoteAddr() net.Addr { return c.sess.RemoteAddr() }

func (c *wtConn) SetDeadline(t time.Time) error      { return c.stream.SetDeadline(t) }
func (c *wtConn) SetReadDeadline(t time.Time) error  { return c.stream.SetReadDeadline(t) }
func (c *wtConn) SetWriteDeadline(t time.Time) error { return c.stream.SetWriteDeadline(t) }
