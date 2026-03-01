package webdial

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"
	"github.com/jpillora/eventsource"
)

func Dial(ctx context.Context, baseURL string) (net.Conn, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	conn, err := dialWS(ctx, baseURL)
	if err == nil {
		return conn, nil
	}
	return dialSSE(ctx, baseURL)
}

func dialWS(ctx context.Context, baseURL string) (net.Conn, error) {
	wsURL := strings.Replace(baseURL, "https://", "wss://", 1)
	wsURL = strings.Replace(wsURL, "http://", "ws://", 1)
	dialer := websocket.Dialer{}
	ws, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return nil, err
	}
	return newWSConn(ws, -1), nil
}

func dialSSE(ctx context.Context, baseURL string) (net.Conn, error) {
	sseURL := baseURL
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sseURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("webdial: sse returned %d", resp.StatusCode)
	}
	decoder := eventsource.NewDecoder(resp.Body)
	var ev eventsource.Event
	if err := decoder.Decode(&ev); err != nil {
		resp.Body.Close()
		return nil, fmt.Errorf("webdial: reading session id: %w", err)
	}
	if ev.Type != "sid" {
		resp.Body.Close()
		return nil, fmt.Errorf("webdial: expected sid event, got %q", ev.Type)
	}
	sid := string(ev.Data)
	return newSSEClientConn(baseURL, sid, resp, decoder, client), nil
}
