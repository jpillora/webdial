package webdial

import (
	"compress/flate"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jpillora/eventsource"
)

func Dial(ctx context.Context, baseURL string) (net.Conn, error) {
	conn, err := dialWS(ctx, baseURL)
	if err == nil {
		return conn, nil
	}
	return dialSSE(ctx, baseURL)
}

func dialWS(ctx context.Context, baseURL string) (net.Conn, error) {
	wsURL, err := websocketURL(baseURL)
	if err != nil {
		return nil, err
	}
	dialer := websocket.Dialer{}
	ws, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return nil, err
	}
	return newWSConn(ws, -1, flate.BestSpeed, 30*time.Second), nil
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
	conn, err := newSSEClientConn(baseURL, sid, resp, decoder, client)
	if err != nil {
		resp.Body.Close()
		return nil, err
	}
	return conn, nil
}

// websocketURL changes only the URL scheme. In particular, URL-like text in
// the query or fragment is left untouched.
func websocketURL(baseURL string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("webdial: parse endpoint URL: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}
	return u.String(), nil
}

// appendURLQueryParam inserts an encoded parameter before any fragment while
// retaining the caller's existing raw query exactly as supplied.
func appendURLQueryParam(baseURL, name, value string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("webdial: parse endpoint URL: %w", err)
	}
	encoded := url.Values{name: []string{value}}.Encode()
	if u.RawQuery == "" {
		u.RawQuery = encoded
	} else {
		u.RawQuery += "&" + encoded
	}
	return u.String(), nil
}
