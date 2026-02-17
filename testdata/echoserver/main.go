package main

import (
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/jpillora/webdial"
)

func main() {
	srv := webdial.NewServer()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	fmt.Println("http://" + ln.Addr().String())
	go http.Serve(ln, srv.Handler())
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
