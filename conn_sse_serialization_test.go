package webdial

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jpillora/eventsource"
	"github.com/stretchr/testify/require"
)

type blockedFlushResponseWriter struct {
	header       http.Header
	body         bytes.Buffer
	mu           sync.Mutex
	flushCount   atomic.Int32
	flushStarted chan struct{}
	releaseFlush chan struct{}
}

func newBlockedFlushResponseWriter() *blockedFlushResponseWriter {
	return &blockedFlushResponseWriter{
		header:       make(http.Header),
		flushStarted: make(chan struct{}),
		releaseFlush: make(chan struct{}),
	}
}

func (w *blockedFlushResponseWriter) Header() http.Header { return w.header }

func (w *blockedFlushResponseWriter) WriteHeader(int) {}

func (w *blockedFlushResponseWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.Write(p)
}

func (w *blockedFlushResponseWriter) Flush() {
	if w.flushCount.Add(1) == 1 {
		close(w.flushStarted)
		<-w.releaseFlush
	}
}

func (w *blockedFlushResponseWriter) events(t *testing.T) []eventsource.Event {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()

	decoder := eventsource.NewDecoder(bytes.NewReader(w.body.Bytes()))
	events := make([]eventsource.Event, 0, 2)
	for {
		var ev eventsource.Event
		if err := decoder.Decode(&ev); err != nil {
			break
		}
		events = append(events, ev)
	}
	return events
}

func TestSSEImmediatePingWaitsForSIDFlush(t *testing.T) {
	srv := NewServer()
	srv.KeepAlive = -1
	defer srv.Close()

	w := newBlockedFlushResponseWriter()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	req.Header.Set("Accept", "text/event-stream")
	handlerDone := make(chan struct{})
	go func() {
		srv.ServeHTTP(w, req)
		close(handlerDone)
	}()

	select {
	case <-w.flushStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the sid event to reach Flush")
	}

	var sid string
	for _, ev := range w.events(t) {
		if ev.Type == "sid" {
			sid = string(ev.Data)
			break
		}
	}
	require.NotEmpty(t, sid)

	postDone := make(chan struct{})
	postRes := httptest.NewRecorder()
	postReq := httptest.NewRequest(http.MethodPost, "/?s="+sid+"&ping=12345", nil)
	go func() {
		srv.ServeHTTP(postRes, postReq)
		close(postDone)
	}()

	select {
	case <-postDone:
		t.Fatal("ping completed while the sid event was still flushing")
	case <-time.After(50 * time.Millisecond):
	}

	close(w.releaseFlush)
	select {
	case <-postDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the serialized ping")
	}
	require.Equal(t, http.StatusNoContent, postRes.Code)

	events := w.events(t)
	require.Len(t, events, 2)
	require.Equal(t, "sid", events[0].Type)
	require.Equal(t, "pong", events[1].Type)
	require.Equal(t, "12345", string(events[1].Data))

	cancel()
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the SSE handler to exit")
	}
}

type failingResponseWriter struct {
	header  http.Header
	err     error
	onWrite func()
}

func (w *failingResponseWriter) Header() http.Header { return w.header }

func (w *failingResponseWriter) WriteHeader(int) {}

func (w *failingResponseWriter) Write([]byte) (int, error) {
	if w.onWrite != nil {
		w.onWrite()
	}
	return 0, w.err
}

func TestSSESIDWriteFailureIsNotAccepted(t *testing.T) {
	srv := NewServer()
	defer srv.Close()

	w := &failingResponseWriter{
		header: make(http.Header),
		err:    errors.New("write failed"),
	}
	var conn *sseServerConn
	w.onWrite = func() {
		srv.sessions.Range(func(_, value any) bool {
			conn = value.(*sseSession).conn
			return false
		})
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept", "text/event-stream")
	srv.ServeHTTP(w, req)

	require.NotNil(t, conn, "session should be registered before its sid is sent")
	require.True(t, conn.closed.Load())
	require.Zero(t, len(srv.acceptCh), "failed SSE connection reached Accept")
	sessionCount := 0
	srv.sessions.Range(func(_, _ any) bool {
		sessionCount++
		return true
	})
	require.Zero(t, sessionCount)

	_, err := conn.Read(make([]byte, 1))
	require.Error(t, err)
	err = conn.deliver(context.Background(), []byte("data"))
	require.Error(t, err)
}
