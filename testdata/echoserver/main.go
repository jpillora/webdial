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
	fmt.Println("http://" + ln.Addr().String() + "/wd/")
	mux := http.NewServeMux()
	mux.Handle("/wd/", srv)
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
