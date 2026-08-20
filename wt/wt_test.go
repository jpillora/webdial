package wt

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"
	"github.com/stretchr/testify/require"

	"github.com/jpillora/webdial"
)

const wtTestTimeout = 5 * time.Second

// selfSignedTLS builds a throwaway ECDSA P-256 certificate for 127.0.0.1. The
// curve and the short validity match what browsers demand of a
// serverCertificateHashes certificate, so the dev story and the tests exercise
// the same shape of certificate.
func selfSignedTLS(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "webdial-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(13 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:              []string{"localhost"},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}},
		NextProtos:   []string{http3.NextProtoH3},
	}, pool
}

type wtTestPair struct {
	client net.Conn
	server net.Conn
	core   *webdial.Server
	wt     *Server
	url    string
	pool   *x509.CertPool
}

// newWTTestPair stands up a real HTTP/3 listener on loopback UDP and returns
// both ends of one established connection, mirroring newSSETestPair in the
// root package. httptest has no HTTP/3 support, so the listener is hand-rolled.
func newWTTestPair(t *testing.T, opts ...func(*Server)) wtTestPair {
	t.Helper()
	srv, base, pool, core := newWTTestServer(t, opts...)

	accepted := make(chan struct {
		conn net.Conn
		err  error
	}, 1)
	go func() {
		conn, err := core.Accept()
		accepted <- struct {
			conn net.Conn
			err  error
		}{conn, err}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), wtTestTimeout)
	defer cancel()
	client, err := (&Dialer{TLSConfig: &tls.Config{RootCAs: pool}}).Dial(ctx, base)
	require.NoError(t, err, "dial WebTransport")
	t.Cleanup(func() { client.Close() })

	var server net.Conn
	select {
	case got := <-accepted:
		require.NoError(t, got.err)
		server = got.conn
	case <-time.After(wtTestTimeout):
		t.Fatal("timed out waiting for server to accept WebTransport connection")
	}
	t.Cleanup(func() { server.Close() })

	return wtTestPair{client: client, server: server, core: core, wt: srv, url: base, pool: pool}
}

// newWTTestServer starts the listener without dialing, for tests that need to
// control the client side themselves.
func newWTTestServer(t *testing.T, opts ...func(*Server)) (*Server, string, *x509.CertPool, *webdial.Server) {
	t.Helper()
	core := webdial.NewServer()
	core.KeepAlive = -1

	tlsConf, pool := selfSignedTLS(t)
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	require.NoError(t, err)

	srv := NewServer(core)
	srv.TLSConfig = tlsConf
	srv.KeepAlive = -1
	for _, opt := range opts {
		opt(srv)
	}
	go srv.Serve(udp)

	t.Cleanup(func() {
		srv.Close()
		core.Close()
		udp.Close()
	})

	return srv, "https://" + udp.LocalAddr().String() + "/wd/", pool, core
}

func TestWTTransport(t *testing.T) {
	pair := newWTTestPair(t)

	// client -> server
	_, err := pair.client.Write([]byte("hello from client"))
	require.NoError(t, err)
	buf := make([]byte, 64)
	n, err := pair.server.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "hello from client", string(buf[:n]))

	// server -> client
	_, err = pair.server.Write([]byte("hello from server"))
	require.NoError(t, err)
	n, err = pair.client.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "hello from server", string(buf[:n]))

	require.NotNil(t, pair.client.LocalAddr())
	require.NotNil(t, pair.client.RemoteAddr())
	require.NotNil(t, pair.server.RemoteAddr())
}
