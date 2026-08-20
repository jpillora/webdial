package webdial

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// requirePushedConnClosed proves Push released the connection it refused. A
// caller that hands over a conn and gets an error back must not be left
// holding something nothing will ever read.
func requirePushedConnClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	_, err := conn.Write([]byte("x"))
	require.Error(t, err, "Push must close the connection it did not deliver")
}

func TestPushDeliversToAccept(t *testing.T) {
	srv := NewServer()
	t.Cleanup(func() { srv.Close() })

	pushed, peer := net.Pipe()
	t.Cleanup(func() { peer.Close() })

	require.NoError(t, srv.Push(context.Background(), pushed))

	accepted, err := srv.Accept()
	require.NoError(t, err)
	require.Same(t, pushed, accepted)
}

func TestPushAfterCloseReturnsErrServerClosed(t *testing.T) {
	srv := NewServer()
	require.NoError(t, srv.Close())

	pushed, peer := net.Pipe()
	t.Cleanup(func() { peer.Close() })

	err := srv.Push(context.Background(), pushed)
	require.ErrorIs(t, err, ErrServerClosed)
	requirePushedConnClosed(t, pushed)
}

func TestPushRespectsContext(t *testing.T) {
	srv := NewServer()
	t.Cleanup(func() { srv.Close() })

	// Fill the accept queue so the next Push has to block.
	for range cap(srv.acceptCh) {
		filler, peer := net.Pipe()
		t.Cleanup(func() { peer.Close() })
		require.NoError(t, srv.Push(context.Background(), filler))
	}

	pushed, peer := net.Pipe()
	t.Cleanup(func() { peer.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- srv.Push(ctx, pushed) }()

	// The push must still be blocked: nothing has drained the queue.
	select {
	case err := <-result:
		t.Fatalf("Push returned early with %v, want it blocked on a full queue", err)
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-result:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(contextTestTimeout):
		t.Fatal("timed out waiting for Push to observe context cancellation")
	}
	requirePushedConnClosed(t, pushed)
}

func TestPushUnblocksOnServerClose(t *testing.T) {
	srv := NewServer()
	for range cap(srv.acceptCh) {
		filler, peer := net.Pipe()
		t.Cleanup(func() { peer.Close() })
		require.NoError(t, srv.Push(context.Background(), filler))
	}

	pushed, peer := net.Pipe()
	t.Cleanup(func() { peer.Close() })

	result := make(chan error, 1)
	go func() { result <- srv.Push(context.Background(), pushed) }()

	// Let the push park on the full queue before closing underneath it.
	time.Sleep(20 * time.Millisecond)
	require.NoError(t, srv.Close())

	select {
	case err := <-result:
		require.ErrorIs(t, err, ErrServerClosed)
	case <-time.After(contextTestTimeout):
		t.Fatal("timed out waiting for Push to observe server close")
	}
	requirePushedConnClosed(t, pushed)
}

func TestAcceptReturnsErrServerClosed(t *testing.T) {
	srv := NewServer()
	require.NoError(t, srv.Close())

	conn, err := srv.Accept()
	require.Nil(t, conn)
	require.ErrorIs(t, err, ErrServerClosed)
	// The message predates ErrServerClosed; keep it stable for callers matching
	// on text rather than identity.
	require.EqualError(t, err, "webdial: server closed")
}
