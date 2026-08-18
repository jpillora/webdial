package webdial

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const postTestTimeout = 2 * time.Second

type countingReadCloser struct {
	reader io.Reader
	reads  int
	bytes  int
}

func (r *countingReadCloser) Read(p []byte) (int, error) {
	r.reads++
	n, err := r.reader.Read(p)
	r.bytes += n
	return n, err
}

func (*countingReadCloser) Close() error { return nil }

type signalingReadCloser struct {
	reader  io.Reader
	started chan struct{}
	once    sync.Once
}

func (r *signalingReadCloser) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	return r.reader.Read(p)
}

func (*signalingReadCloser) Close() error { return nil }

func newPostTestSession(t *testing.T, srv *Server) (*sseServerConn, string) {
	t.Helper()
	pr, pw := io.Pipe()
	conn := &sseServerConn{
		readPipe:  pr,
		writePipe: pw,
		closeCh:   make(chan struct{}),
	}
	sid := "post-test-session"
	srv.sessions.Store(sid, newSSESession(conn))
	t.Cleanup(func() {
		srv.sessions.Delete(sid)
		_ = pr.Close()
		_ = pw.Close()
	})
	return conn, sid
}

func servePostAsync(srv *Server, req *http.Request) (<-chan *httptest.ResponseRecorder, <-chan struct{}) {
	result := make(chan *httptest.ResponseRecorder, 1)
	done := make(chan struct{})
	go func() {
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		result <- w
		close(done)
	}()
	return result, done
}

func waitPostResult(t *testing.T, result <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(postTestTimeout):
		t.Fatal("timed out waiting for POST handler")
		return nil
	}
}

func TestServerMaxPostBytesDefaults(t *testing.T) {
	srv := NewServer()
	require.Equal(t, defaultMaxPostBytes, srv.maxPostBytes())

	srv.MaxPostBytes = 4 << 20
	require.EqualValues(t, 4<<20, srv.maxPostBytes())

	srv.MaxPostBytes = -1
	require.EqualValues(t, -1, srv.maxPostBytes())
}

func TestSSEPostRejectsOversizedContentLengthWithoutReading(t *testing.T) {
	srv := NewServer()
	srv.MaxPostBytes = 4
	_, sid := newPostTestSession(t, srv)
	body := &countingReadCloser{reader: bytes.NewReader([]byte("12345"))}
	req := httptest.NewRequest(http.MethodPost, "/?s="+sid, body)
	req.ContentLength = 5
	w := httptest.NewRecorder()

	srv.ServeHTTP(w, req)

	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	require.Zero(t, body.reads, "declared oversized body should be rejected before reading")
}

func TestSSEPostRejectsOversizedChunkedBody(t *testing.T) {
	srv := NewServer()
	srv.MaxPostBytes = 4
	conn, sid := newPostTestSession(t, srv)
	body := &countingReadCloser{reader: bytes.NewReader([]byte("12345"))}
	req := httptest.NewRequest(http.MethodPost, "/?s="+sid, body)
	req.ContentLength = -1
	w := httptest.NewRecorder()

	srv.ServeHTTP(w, req)

	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	require.LessOrEqual(t, body.bytes, 5, "limiter read beyond limit plus probe byte")

	// Rejection happens before delivery, so the session remains usable and no
	// prefix from the rejected request contaminates the byte stream.
	valid := []byte{0x00, 0xff, 0x41, 0x80}
	validReq := httptest.NewRequest(http.MethodPost, "/?s="+sid, bytes.NewReader(valid))
	result, _ := servePostAsync(srv, validReq)
	got := make([]byte, len(valid))
	_, err := io.ReadFull(conn.readPipe, got)
	require.NoError(t, err)
	require.Equal(t, valid, got)
	require.Equal(t, http.StatusNoContent, waitPostResult(t, result).Code)
}

func TestSSEPostDeliversBinaryPayloadAtLimit(t *testing.T) {
	srv := NewServer()
	srv.MaxPostBytes = 8
	conn, sid := newPostTestSession(t, srv)
	payload := []byte{0x00, 0xff, 0x01, 0x80, 0x7f, 0x42, 0x00, 0xfe}
	body := &signalingReadCloser{
		reader:  bytes.NewReader(payload),
		started: make(chan struct{}),
	}
	req := httptest.NewRequest(http.MethodPost, "/?s="+sid, body)
	req.ContentLength = -1 // exercise bounded chunked ingestion at the limit
	result, _ := servePostAsync(srv, req)

	got := make([]byte, len(payload))
	_, err := io.ReadFull(conn.readPipe, got)
	require.NoError(t, err)
	require.Equal(t, payload, got)
	require.Equal(t, http.StatusNoContent, waitPostResult(t, result).Code)
}

func TestSSEPostCancellationUnblocksSlowReader(t *testing.T) {
	srv := NewServer()
	srv.MaxPostBytes = 64
	conn, sid := newPostTestSession(t, srv)
	started := make(chan struct{})
	body := &signalingReadCloser{
		reader:  bytes.NewReader(bytes.Repeat([]byte("x"), 64)),
		started: started,
	}
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/?s="+sid, body).WithContext(ctx)
	req.ContentLength = 64
	result, done := servePostAsync(srv, req)

	select {
	case <-started:
	case <-time.After(postTestTimeout):
		t.Fatal("timed out waiting for POST body read")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(postTestTimeout):
		t.Fatal("canceled POST remained blocked on slow application reader")
	}
	require.Equal(t, http.StatusRequestTimeout, (<-result).Code)
	_, err := conn.readPipe.Read(make([]byte, 1))
	require.ErrorIs(t, err, context.Canceled)
}

func TestSSEPostReturnsDeliveryError(t *testing.T) {
	srv := NewServer()
	conn, sid := newPostTestSession(t, srv)
	require.NoError(t, conn.readPipe.Close())
	req := httptest.NewRequest(http.MethodPost, "/?s="+sid, bytes.NewReader([]byte("data")))
	w := httptest.NewRecorder()

	srv.ServeHTTP(w, req)

	require.Equal(t, http.StatusGone, w.Code)
}

func TestSSEConcurrentPostsAreSerialized(t *testing.T) {
	srv := NewServer()
	srv.MaxPostBytes = 128 << 10
	conn, sid := newPostTestSession(t, srv)
	first := bytes.Repeat([]byte{0xa1}, 64<<10)
	second := bytes.Repeat([]byte{0xb2}, 64<<10)
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	firstReq := httptest.NewRequest(http.MethodPost, "/?s="+sid, &signalingReadCloser{
		reader:  bytes.NewReader(first),
		started: firstStarted,
	})
	firstReq.ContentLength = int64(len(first))
	secondReq := httptest.NewRequest(http.MethodPost, "/?s="+sid, &signalingReadCloser{
		reader:  bytes.NewReader(second),
		started: secondStarted,
	})
	secondReq.ContentLength = int64(len(second))

	firstResult, _ := servePostAsync(srv, firstReq)
	select {
	case <-firstStarted:
	case <-time.After(postTestTimeout):
		t.Fatal("timed out waiting for first POST to start")
	}
	secondResult, _ := servePostAsync(srv, secondReq)
	select {
	case <-secondStarted:
		t.Fatal("second POST body was read before first POST completed")
	case <-time.After(50 * time.Millisecond):
	}

	gotFirst := make([]byte, len(first))
	_, err := io.ReadFull(conn.readPipe, gotFirst)
	require.NoError(t, err)
	require.Equal(t, first, gotFirst)
	require.Equal(t, http.StatusNoContent, waitPostResult(t, firstResult).Code)

	select {
	case <-secondStarted:
	case <-time.After(postTestTimeout):
		t.Fatal("second POST did not start after first POST completed")
	}
	gotSecond := make([]byte, len(second))
	_, err = io.ReadFull(conn.readPipe, gotSecond)
	require.NoError(t, err)
	require.Equal(t, second, gotSecond)
	require.Equal(t, http.StatusNoContent, waitPostResult(t, secondResult).Code)
}

func TestGoSSEClientSurfacesPostLimit(t *testing.T) {
	srv := NewServer()
	srv.MaxPostBytes = 4
	defer srv.Close()
	ts := httptest.NewServer(srv)
	defer ts.Close()

	clientConn, err := dialSSE(context.Background(), ts.URL)
	require.NoError(t, err)
	defer clientConn.Close()
	serverConn, err := srv.Accept()
	require.NoError(t, err)
	defer serverConn.Close()

	n, err := clientConn.Write([]byte("12345"))
	require.Zero(t, n)
	require.ErrorContains(t, err, "post returned 413")
}
