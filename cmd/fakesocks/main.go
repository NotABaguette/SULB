// fakesocks is a test-only SOCKS5 proxy that forwards every CONNECT to a
// fixed destination. Used by scripts/smoke.sh to simulate uplinks.
package main

import (
	"flag"
	"io"
	"log"
	"net"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:1081", "listen address")
	dest := flag.String("dest", "", "fixed destination host:port")
	flag.Parse()
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("fakesocks listening on %s -> %s", *listen, *dest)
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go handle(c, *dest)
	}
}

func handle(c net.Conn, dest string) {
	defer c.Close()
	// greeting: VER NMETHODS METHODS — accept no-auth or anything
	buf := make([]byte, 64)
	if _, err := io.ReadFull(c, buf[:2]); err != nil {
		return
	}
	if _, err := io.ReadFull(c, buf[:buf[1]]); err != nil {
		return
	}
	c.Write([]byte{5, 0})
	// request: VER CMD RSV ATYP ... — read until port
	if _, err := io.ReadFull(c, buf[:4]); err != nil {
		return
	}
	switch buf[3] {
	case 1:
		if _, err := io.ReadFull(c, buf[:4+2]); err != nil {
			return
		}
	case 3:
		if _, err := io.ReadFull(c, buf[:1]); err != nil {
			return
		}
		if _, err := io.ReadFull(c, buf[:buf[0]+2]); err != nil {
			return
		}
	case 4:
		if _, err := io.ReadFull(c, buf[:16+2]); err != nil {
			return
		}
	default:
		return
	}
	rc, err := net.Dial("tcp", dest)
	if err != nil {
		log.Printf("dest dial failed: %v", err)
		return // closes client conn -> upstream probe fails
	}
	defer rc.Close()
	c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	go io.Copy(rc, c)
	io.Copy(c, rc)
}
