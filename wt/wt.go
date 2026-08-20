// Package wt serves and dials webdial connections over WebTransport (HTTP/3).
//
// It is a separate package so that programs using only the WebSocket and SSE
// transports do not link quic-go. Nothing in the root webdial package imports
// this one; the dependency runs strictly the other way.
//
// A session carries exactly one client-initiated bidirectional stream, and
// that stream is the net.Conn. Latency probes travel over WebTransport
// datagrams as "ping:<ts>"/"pong:<ts>", the same control vocabulary the
// WebSocket transport puts in text frames.
package wt

import (
	"crypto/tls"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

// acceptStreamTimeout bounds the wait for the client's first stream. A peer
// that completes the CONNECT handshake and then opens nothing would otherwise
// park a handler goroutine for the lifetime of the session.
const acceptStreamTimeout = 10 * time.Second

// closeLingerTimeout bounds how long a closed connection keeps its session
// alive so the last write can drain. It mirrors a conventional TCP linger.
const closeLingerTimeout = 5 * time.Second

// webtransportURL changes only the URL scheme, leaving the path, raw query and
// fragment exactly as supplied.
//
// Unlike the WebSocket equivalent in the root package, which passes unknown
// schemes through untouched, this rejects anything that is not http or https:
// WebTransport runs over HTTP/3 only, and an unrecognised scheme otherwise
// surfaces as an opaque failure deep inside the QUIC dial.
func webtransportURL(baseURL string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("webdial: parse endpoint URL: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		u.Scheme = "https"
	default:
		return "", fmt.Errorf("webdial: webtransport requires an https URL, got %q", u.Scheme)
	}
	return u.String(), nil
}

// withH3ALPN returns a copy of cfg guaranteed to offer the h3 ALPN.
// WebTransport cannot negotiate anything else, and a config carrying only
// unrelated protocols fails the handshake obscurely.
func withH3ALPN(cfg *tls.Config) *tls.Config {
	var out *tls.Config
	if cfg != nil {
		out = cfg.Clone()
	} else {
		out = &tls.Config{}
	}
	if !slices.Contains(out.NextProtos, http3.NextProtoH3) {
		out.NextProtos = append(out.NextProtos, http3.NextProtoH3)
	}
	return out
}

// quicConfig returns a copy of base carrying the extensions WebTransport
// requires. webtransport-go fills these in itself only when the QUIC config is
// nil, so supplying one and omitting either flag is an error at dial time
// rather than a silent downgrade. The caller's value is never mutated.
func quicConfig(base *quic.Config, keepAlive time.Duration) *quic.Config {
	var cfg quic.Config
	if base != nil {
		cfg = *base
	}
	cfg.EnableDatagrams = true
	cfg.EnableStreamResetPartialDelivery = true
	if keepAlive > 0 {
		cfg.KeepAlivePeriod = keepAlive
	}
	return &cfg
}
