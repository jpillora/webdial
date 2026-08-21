package webdial

import (
	"bytes"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// pongQueue is deliberately shallow: a peer that has not drained one pong is
// not going to benefit from us buffering more.
const pongQueue = 4

// maxControlFrame bounds an application-level text control frame. They carry
// "ping:<ts>" and nothing else, so this is generous.
const maxControlFrame = 1 << 10

// wsReadResult is one filled read request handed back by the pump. data aliases
// the pump's buffer and stays valid until the next request is sent, which Read
// only does once it has copied every byte out.
type wsReadResult struct {
	data []byte
	err  error
}

type wsConn struct {
	ws           *websocket.Conn
	writeMu      sync.Mutex
	writeTimeout time.Duration
	userDeadline atomic.Bool
	pongs        chan []byte
	done         chan struct{}
	closeOnce    sync.Once
	terminalMu   sync.Mutex
	terminalErr  error

	// Read side. mu serialises callers and guards remainder and pending.
	mu           sync.Mutex
	remainder    []byte
	pending      bool
	readDeadline connDeadline
	reqs         chan int
	resp         chan wsReadResult

	// Owned exclusively by the pump goroutine; no lock, and nothing else may
	// touch them. gorilla permits at most one goroutine in its read methods,
	// and a message reader is invalidated the moment NextReader advances.
	reader io.Reader
	buf    []byte
}

func newWSConn(ws *websocket.Conn, keepAlive time.Duration, compressionLevel int, writeTimeout time.Duration) net.Conn {
	ws.EnableWriteCompression(true)
	_ = ws.SetCompressionLevel(compressionLevel)
	c := &wsConn{
		ws:           ws,
		writeTimeout: writeTimeout,
		pongs:        make(chan []byte, pongQueue),
		done:         make(chan struct{}),
		readDeadline: newConnDeadline(),
		reqs:         make(chan int, 1),
		resp:         make(chan wsReadResult, 1),
	}
	go c.controlLoop(keepAlive)
	go c.pump()
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

// pump is the only goroutine that touches gorilla's read side.
//
// Two things follow from that. Deadlines no longer reach the socket, so a
// fired one can no longer latch gorilla's readErr and kill the connection —
// os.ErrDeadlineExceeded means "not yet", as net.Conn requires. And the peer's
// "ping:<ts>" frames are answered whether or not the application happens to be
// inside Read, so a server busy elsewhere is not torn down by its own client's
// staleness watchdog.
//
// The caller lends a size rather than a buffer: handing the caller's slice to
// the pump would race once that caller abandons it on a timeout, and handing
// gorilla's message reader out would both race with NextReader and, when the
// pump advanced, quietly truncate the message into a clean io.EOF.
func (c *wsConn) pump() {
	for {
		if c.reader == nil {
			mt, r, err := c.ws.NextReader()
			if err != nil {
				// gorilla latches read errors and panics after a thousand
				// consecutive ones, so the first is always terminal.
				c.shutdown(err)
				return
			}
			if mt == websocket.TextMessage {
				// Bounded: these are tiny "ping:<ts>" control frames, and an
				// unbounded ReadAll here lets one peer allocate whatever it
				// declares.
				ctrl, _ := io.ReadAll(io.LimitReader(r, maxControlFrame))
				c.handleControl(ctrl)
				continue
			}
			c.reader = r
		}
		var size int
		select {
		case size = <-c.reqs:
		case <-c.done:
			return
		}
		if cap(c.buf) < size {
			c.buf = make([]byte, size)
		}
		n, err := c.fill(c.buf[:size])
		if err == io.EOF {
			// End of message, not end of connection. A zero-length message
			// lands here with n == 0; Read asks again rather than reporting it.
			c.reader = nil
			err = nil
		}
		select {
		case c.resp <- wsReadResult{data: c.buf[:n], err: err}:
		case <-c.done:
			return
		}
		if err != nil {
			// Published after the bytes that precede it, so Read hands those
			// to the caller before reporting the failure.
			c.shutdown(err)
			return
		}
	}
}

func (c *wsConn) Read(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(b) == 0 {
		return 0, nil
	}
	for {
		if len(c.remainder) > 0 {
			n := copy(b, c.remainder)
			c.remainder = c.remainder[n:]
			return n, nil
		}
		// A result the pump has already published owns bytes that precede any
		// terminal error, so collect it before reporting one.
		if c.pending {
			select {
			case res := <-c.resp:
				c.collect(res)
				continue
			default:
			}
		}
		if err := c.terminalError(); err != nil {
			return 0, err
		}
		// One request is outstanding at a time. A request that outlived its
		// Read — the caller timed out while the pump was still filling — is
		// never reissued, so the pump can never be writing into its buffer
		// while bytes the caller has not copied out are still in it.
		if !c.pending {
			select {
			case c.reqs <- len(b):
				c.pending = true
			case <-c.done:
				continue
			}
		}
		deadline := c.readDeadline.snapshot()
		if deadline.expired {
			if c.readDeadline.expired() {
				return 0, os.ErrDeadlineExceeded
			}
			continue
		}
		select {
		case res := <-c.resp:
			deadline.stop()
			c.collect(res)
		case <-c.done:
			deadline.stop()
			continue
		case <-deadline.timerC():
			// The pump keeps filling its own buffer, so nothing is lost and the
			// connection survives: the next Read collects the completed result.
			if c.readDeadline.expired() {
				return 0, os.ErrDeadlineExceeded
			}
		case <-deadline.changed:
			deadline.stop()
		}
	}
}

// collect takes ownership of a pump result. Caller must hold mu.
func (c *wsConn) collect(res wsReadResult) {
	c.pending = false
	c.remainder = res.data
	if res.err != nil {
		c.setTerminalError(res.err)
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
// A message larger than b still spans Reads, so consumers treating the conn as
// a byte stream keep working — but note this blocks until b is full or the
// message ends, where a plain socket returns as soon as any bytes arrive.
// Consumers that need whole messages must pass a buffer at least as large as
// their maximum frame. One consequence: unlike a TCP conn, a read deadline can
// expire while bytes of the message have already arrived.
func (c *wsConn) fill(b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := c.reader.Read(b[n:])
		n += m
		if err != nil {
			return n, err
		}
		if m == 0 {
			// A reader making no progress without an error would spin here, and
			// every known consumer treats a short read as "try again" — so fail
			// loudly rather than hand back a silent hang.
			return n, io.ErrNoProgress
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
	c.shutdown(net.ErrClosed)
	return nil
}

// shutdown records the connection's terminal error, releases the pump and the
// control loop, and closes the socket. Recording the error first is what makes
// a read after a local Close deterministic: the "use of closed network
// connection" that closing the socket provokes in the pump arrives second and
// loses to the error already stored.
func (c *wsConn) shutdown(err error) {
	if err == nil {
		err = net.ErrClosed
	}
	c.setTerminalError(err)
	c.closeOnce.Do(func() {
		close(c.done)
		c.ws.Close()
	})
}

func (c *wsConn) setTerminalError(err error) {
	c.terminalMu.Lock()
	if c.terminalErr == nil {
		c.terminalErr = err
	}
	c.terminalMu.Unlock()
}

func (c *wsConn) terminalError() error {
	c.terminalMu.Lock()
	defer c.terminalMu.Unlock()
	return c.terminalErr
}

func (c *wsConn) closed() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func (c *wsConn) LocalAddr() net.Addr  { return c.ws.LocalAddr() }
func (c *wsConn) RemoteAddr() net.Addr { return c.ws.RemoteAddr() }

func (c *wsConn) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}
	return c.SetWriteDeadline(t)
}

// SetReadDeadline no longer reaches the socket. gorilla latches whatever error
// its read side produces, so letting a deadline fire there would end the
// connection permanently instead of merely ending one Read.
func (c *wsConn) SetReadDeadline(t time.Time) error {
	if c.closed() {
		return net.ErrClosed
	}
	c.readDeadline.set(t)
	return nil
}

// SetWriteDeadline does reach the socket, deliberately. A write that times out
// has already put a partial frame on the wire, leaving the peer's parser
// desynchronised, so there is no resume point and gorilla's latching is right.
func (c *wsConn) SetWriteDeadline(t time.Time) error {
	if c.closed() {
		return net.ErrClosed
	}
	c.userDeadline.Store(!t.IsZero())
	return c.ws.SetWriteDeadline(t)
}
