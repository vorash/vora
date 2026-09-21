// Command netserve is a test helper: it listens on a TCP address inside a
// network namespace (launched with nsenter) and replies with a fixed banner.
// It lives under testdata so the go tool never builds it as part of the module.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
)

func main() {
	addr := flag.String("addr", "", "TCP address to listen on")
	flag.Parse()
	if *addr == "" {
		fmt.Fprintln(os.Stderr, "netserve: -addr is required")
		os.Exit(2)
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("netserve: listen %s: %v", *addr, err)
	}
	fmt.Fprintln(os.Stderr, "ready")
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = c.Write([]byte("NETSERVE\n"))
		_ = c.Close()
	}
}
