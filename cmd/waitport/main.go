// Command waitport reports whether a TCP address accepts a connection.
// It replaces `nc -z` in the Makefile: nc is missing from many CI images and
// its flags differ between the BSD, GNU and busybox variants.
//
//	go run ./cmd/waitport 127.0.0.1:7301 [timeout]
//
// Exit code 0 means the port is accepting connections, 1 means it is not.
package main

import (
	"fmt"
	"net"
	"os"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: waitport host:port [timeout]")
		os.Exit(2)
	}

	timeout := 2 * time.Second
	if len(os.Args) > 2 {
		d, err := time.ParseDuration(os.Args[2])
		if err != nil {
			fmt.Fprintf(os.Stderr, "waitport: bad timeout %q: %v\n", os.Args[2], err)
			os.Exit(2)
		}
		timeout = d
	}

	conn, err := net.DialTimeout("tcp", os.Args[1], timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "waitport: %v\n", err)
		os.Exit(1)
	}
	_ = conn.Close()
}
