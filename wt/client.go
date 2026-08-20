package wt

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"
)

// Dialer configures WebTransport dials. The zero value is usable.
type Dialer struct {
	// TLSConfig configures the QUIC handshake. Use it to trust a development
	// certificate. Nil means the host's root CAs.
	//
	// The h3 ALPN is appended when NextProtos does not already contain it:
	// WebTransport cannot negotiate anything else, and a config carrying only
	// unrelated protocols would otherwise fail the handshake obscurely.
	TLSConfig *tls.Config
	// QUICConfig, when non-nil, is the base QUIC configuration. The extensions
	// WebTransport requires are applied to a copy.
	QUICConfig *quic.Config
	// KeepAlive sets the QUIC keep-alive period. Zero leaves it to QUICConfig.
	KeepAlive time.Duration
}

// Dial establishes a webdial connection over WebTransport. baseURL must be an
// http or https URL; http is promoted to https, since WebTransport has no
// cleartext form. Note that the HTTP/3 endpoint commonly listens on a
// different port from the HTTP endpoint, even when the path is identical.
//
// As with net.Dialer.DialContext, ctx governs establishment only: it does not
// carry over to the returned connection.
func (d *Dialer) Dial(ctx context.Context, baseURL string) (net.Conn, error) {
	wtURL, err := webtransportURL(baseURL)
	if err != nil {
		return nil, err
	}
	tr := &webtransport.Transport{
		TLSClientConfig: withH3ALPN(d.TLSConfig),
		QUICConfig:      quicConfig(d.QUICConfig, d.KeepAlive),
		Config: &webtransport.Config{
			MaxIncomingStreams:    defaultMaxIncomingStreams,
			MaxIncomingUniStreams: defaultMaxIncomingStreams,
			MaxIncomingData:       defaultMaxIncomingData,
		},
	}
	_, sess, err := tr.Dial(ctx, wtURL, nil)
	if err != nil {
		tr.Close()
		return nil, fmt.Errorf("webdial: webtransport dial: %w", err)
	}
	// The server accepts the first stream on the session and treats it as the
	// connection, so opening it is part of establishment rather than something
	// deferred to the first Write.
	stream, err := sess.OpenStreamSync(ctx)
	if err != nil {
		sess.CloseWithError(0, "")
		tr.Close()
		return nil, fmt.Errorf("webdial: webtransport open stream: %w", err)
	}
	// Opening a stream is purely local, and the WebTransport stream header is
	// buffered until the first write, so the server learns nothing until we
	// send. This empty write flushes that header and no payload, which is what
	// makes the stream visible to AcceptStream. Without it a server-speaks-
	// first protocol would deadlock: each side would be waiting to read.
	if _, err := stream.Write(nil); err != nil {
		sess.CloseWithError(0, "")
		tr.Close()
		return nil, fmt.Errorf("webdial: webtransport open stream: %w", err)
	}
	return newWTConn(sess, stream), nil
}

// Dial calls (&Dialer{}).Dial.
func Dial(ctx context.Context, baseURL string) (net.Conn, error) {
	return (&Dialer{}).Dial(ctx, baseURL)
}
