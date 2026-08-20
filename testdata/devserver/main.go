// Command devserver serves the example page against a live webdial server.
//
// It listens twice: TCP for the page plus the WebSocket and SSE transports,
// and UDP/HTTP/3 for WebTransport. Both listeners feed one Accept loop.
//
// WebTransport needs HTTPS, and a self-signed certificate would normally be
// refused. Browsers make an exception for a certificate pinned by hash, so the
// server mints an ECDSA P-256 certificate and publishes its SHA-256 at
// /wt.json for the page to pass to the WebTransport constructor.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"time"

	"github.com/jpillora/webdial"
	"github.com/jpillora/webdial/wt"
)

// devCertValidity stays under the 14 day ceiling browsers impose on
// certificates pinned with serverCertificateHashes.
const devCertValidity = 13 * 24 * time.Hour

func devCertificate() (*tls.Config, string, error) {
	// Must be ECDSA P-256: browsers reject RSA certificates pinned by hash.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "webdial devserver"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(devCertValidity),
		// An end-entity certificate: browsers refuse to pin a CA certificate
		// by hash and serve it as the leaf.
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:              []string{"localhost"},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(der)
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
	}, hex.EncodeToString(sum[:]), nil
}

func main() {
	srv := webdial.NewServer()

	tlsConf, certHash, err := devCertificate()
	if err != nil {
		panic(err)
	}

	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		panic(err)
	}
	wtURL := "https://" + udp.LocalAddr().String() + "/wd/"

	mux := http.NewServeMux()
	mux.Handle("/wd/", srv)
	mux.HandleFunc("/wt.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"url": wtURL, "hash": certHash})
	})
	mux.Handle("/", http.FileServer(http.Dir(".")))

	wtSrv := wt.NewServer(srv)
	wtSrv.TLSConfig = tlsConf
	h3mux := http.NewServeMux()
	h3mux.Handle("/wd/", wtSrv)
	wtSrv.Handler = h3mux
	go wtSrv.Serve(udp)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	// The first line stays an http:// URL: existing tooling reads it to find
	// the server.
	fmt.Println("http://" + ln.Addr().String())
	fmt.Println(wtURL)
	go http.Serve(ln, mux)

	for {
		conn, err := srv.Accept()
		if err != nil {
			break
		}
		go func() {
			defer conn.Close()
			io.Copy(conn, conn)
		}()
	}
}
