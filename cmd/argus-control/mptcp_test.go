package main

import (
	"net"
	"testing"
)

// CVE-2026-98432 is a use-after-free in the Linux IPv4 TCP handshake. The only
// known exploit needs an MPTCP listener, and Go has opened one for every
// net.Listen("tcp") since 1.24. go.mod sets godebug multipathtcp=0 so the
// control plane's listeners are plain TCP whatever the host's kernel allows.
func TestListenersAreNotMultipath(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- c
	}()

	// The client asks for MPTCP, so the connection is multipath if and only if
	// the listener accepted it as such.
	d := net.Dialer{}
	d.SetMultipathTCP(true)
	client, err := d.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	server, ok := <-accepted
	if !ok {
		t.Fatal("accept failed")
	}
	defer server.Close()

	mp, err := server.(*net.TCPConn).MultipathTCP()
	if err != nil {
		t.Fatal(err)
	}
	if mp {
		t.Fatal("control plane listener accepted an MPTCP connection; " +
			"go.mod must keep `godebug multipathtcp=0` (CVE-2026-98432)")
	}
}
