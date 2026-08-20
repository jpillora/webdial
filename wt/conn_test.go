package wt

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/quic-go/webtransport-go"
	"github.com/stretchr/testify/require"
)

// dialRawSession establishes a session without the wtConn wrapper, so a test
// can drive the wire directly. The stream is opened and its header flushed
// because the server only builds its connection once a stream arrives.
func dialRawSession(t *testing.T, base string, pool *x509.CertPool) (*webtransport.Session, *webtransport.Stream) {
	t.Helper()
	tr := &webtransport.Transport{
		TLSClientConfig: withH3ALPN(&tls.Config{RootCAs: pool}),
		QUICConfig:      quicConfig(nil, 0),
		Config: &webtransport.Config{
			MaxIncomingStreams:    defaultMaxIncomingStreams,
			MaxIncomingUniStreams: defaultMaxIncomingStreams,
			MaxIncomingData:       defaultMaxIncomingData,
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), wtTestTimeout)
	defer cancel()
	_, sess, err := tr.Dial(ctx, base, nil)
	require.NoError(t, err)
	t.Cleanup(func() { tr.Close() })

	stream, err := sess.OpenStreamSync(ctx)
	require.NoError(t, err)
	_, err = stream.Write(nil)
	require.NoError(t, err)
	return sess, stream
}

func TestWTPingPong(t *testing.T) {
	_, base, pool, core := newWTTestServer(t)
	go func() {
		conn, err := core.Accept()
		if err == nil {
			t.Cleanup(func() { conn.Close() })
		}
	}()

	sess, _ := dialRawSession(t, base, pool)

	require.NoError(t, sess.SendDatagram([]byte("ping:12345")))
	ctx, cancel := context.WithTimeout(context.Background(), wtTestTimeout)
	defer cancel()
	got, err := sess.ReceiveDatagram(ctx)
	require.NoError(t, err)
	require.Equal(t, "pong:12345", string(got))
}

func TestWTUnknownDatagramIgnored(t *testing.T) {
	_, base, pool, core := newWTTestServer(t)
	go func() {
		conn, err := core.Accept()
		if err == nil {
			t.Cleanup(func() { conn.Close() })
		}
	}()

	sess, _ := dialRawSession(t, base, pool)

	// An unrecognised datagram must neither crash the control loop nor draw a
	// reply, so the ping that follows it is what proves the loop survived.
	require.NoError(t, sess.SendDatagram([]byte("something-else")))
	require.NoError(t, sess.SendDatagram([]byte("ping:7")))

	ctx, cancel := context.WithTimeout(context.Background(), wtTestTimeout)
	defer cancel()
	got, err := sess.ReceiveDatagram(ctx)
	require.NoError(t, err)
	require.Equal(t, "pong:7", string(got))
}

func TestWTReadDeadline(t *testing.T) {
	pair := newWTTestPair(t)

	require.NoError(t, pair.client.SetReadDeadline(time.Now().Add(40*time.Millisecond)))
	_, err := pair.client.Read(make([]byte, 1))
	require.ErrorIs(t, err, os.ErrDeadlineExceeded)
	var netErr net.Error
	require.ErrorAs(t, err, &netErr)
	require.True(t, netErr.Timeout())

	// Clearing the deadline must leave the connection fully usable: a timeout
	// is not a terminal condition.
	require.NoError(t, pair.client.SetReadDeadline(time.Time{}))
	_, err = pair.server.Write([]byte("still alive"))
	require.NoError(t, err)
	buf := make([]byte, 32)
	n, err := pair.client.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "still alive", string(buf[:n]))
}

func TestWTWriteDeadline(t *testing.T) {
	pair := newWTTestPair(t)

	// Nothing drains the server end, so a write larger than the peer's
	// flow-control window blocks and the deadline is what ends it.
	require.NoError(t, pair.client.SetWriteDeadline(time.Now().Add(60*time.Millisecond)))
	_, err := pair.client.Write(make([]byte, 4*defaultMaxIncomingData))
	require.ErrorIs(t, err, os.ErrDeadlineExceeded)
	var netErr net.Error
	require.ErrorAs(t, err, &netErr)
	require.True(t, netErr.Timeout())

	// The connection survives the timeout. How much of the oversized write
	// landed is not predictable, so the check is that the other direction
	// still carries data once the deadline is cleared.
	require.NoError(t, pair.client.SetWriteDeadline(time.Time{}))
	go io.Copy(io.Discard, pair.server)

	_, err = pair.server.Write([]byte("still alive"))
	require.NoError(t, err)
	buf := make([]byte, 32)
	n, err := pair.client.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "still alive", string(buf[:n]))
}

func TestWTCloseUnblocksBlockedRead(t *testing.T) {
	pair := newWTTestPair(t)

	errCh := make(chan error, 1)
	go func() {
		_, err := pair.client.Read(make([]byte, 1))
		errCh <- err
	}()
	// Let the read park before closing underneath it.
	time.Sleep(50 * time.Millisecond)
	require.NoError(t, pair.client.Close())

	select {
	case err := <-errCh:
		require.ErrorIs(t, err, net.ErrClosed, "a local close must read back as net.ErrClosed, not a raw QUIC error")
	case <-time.After(wtTestTimeout):
		t.Fatal("timed out waiting for Close to unblock a parked Read")
	}
}

func TestWTWriteAfterCloseReturnsErrClosed(t *testing.T) {
	pair := newWTTestPair(t)
	require.NoError(t, pair.client.Close())
	_, err := pair.client.Write([]byte("x"))
	require.ErrorIs(t, err, net.ErrClosed)
}

func TestWTCloseIsIdempotent(t *testing.T) {
	pair := newWTTestPair(t)
	require.NoError(t, pair.client.Close())
	require.NoError(t, pair.client.Close())
}

func TestWTRemoteCloseReturnsEOF(t *testing.T) {
	pair := newWTTestPair(t)

	_, err := pair.server.Write([]byte("bye"))
	require.NoError(t, err)
	require.NoError(t, pair.server.Close())

	buf := make([]byte, 16)
	n, err := pair.client.Read(buf)
	if n > 0 {
		require.Equal(t, "bye", string(buf[:n]))
		_, err = pair.client.Read(buf)
	}
	require.Error(t, err)
	require.NotErrorIs(t, err, net.ErrClosed, "a remote teardown is not a local close")
}

// TestWTLargeTransfer guards the flow-control settings. WebTransport advertises
// its own initial window, and getting it wrong stalls a transfer partway
// through rather than failing outright.
func TestWTLargeTransfer(t *testing.T) {
	pair := newWTTestPair(t)

	payload := bytes.Repeat([]byte("webdial"), 3*defaultMaxIncomingData/7)
	go func() {
		pair.client.Write(payload)
		pair.client.Close()
	}()

	done := make(chan []byte, 1)
	go func() {
		got, _ := io.ReadAll(pair.server)
		done <- got
	}()

	select {
	case got := <-done:
		require.Equal(t, len(payload), len(got))
		require.True(t, bytes.Equal(payload, got))
	case <-time.After(3 * wtTestTimeout):
		t.Fatal("timed out transferring more than the initial flow-control window")
	}
}
