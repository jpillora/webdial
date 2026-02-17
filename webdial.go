package webdial

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"time"
)

type addr struct {
	transport string
	url       string
}

func (a addr) Network() string { return "webdial-" + a.transport }
func (a addr) String() string  { return a.url }

func generateSessionID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

type noopDeadline struct{}

func (noopDeadline) SetDeadline(t time.Time) error      { return nil }
func (noopDeadline) SetReadDeadline(t time.Time) error  { return nil }
func (noopDeadline) SetWriteDeadline(t time.Time) error { return nil }

var _ net.Conn = (*wsConn)(nil)
var _ net.Conn = (*sseClientConn)(nil)
var _ net.Conn = (*sseServerConn)(nil)
