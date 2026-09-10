// Command gencerts writes a development PKI.
//
// Development only. Production uses a real certificate authority.
package main

import (
	"fmt"
	"os"

	"github.com/gsoultan/argus/internal/tlsconfig"
)

func main() {
	dir := "dev/certs"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	// Cover both the name and the address the listeners are reached by: a
	// certificate that omits 127.0.0.1 is the usual reason local TLS gets
	// abandoned.
	//
	// Anything after the directory is an extra name or address. An agent inside
	// a container reaches the host on its bridge gateway address, which differs
	// per runtime and per machine, so it cannot be listed here -- and a
	// certificate that omits it is why the dev agent ended up configured to
	// talk plain HTTP instead. gen-certs.sh discovers it and passes it in.
	hosts := []string{"localhost", "argus.local", "127.0.0.1", "::1", "host.containers.internal"}
	if len(os.Args) > 2 {
		hosts = append(hosts, os.Args[2:]...)
	}
	if err := tlsconfig.GenerateDevPKI(dir, hosts); err != nil {
		fmt.Fprintf(os.Stderr, "gencerts: %v\n", err)
		os.Exit(1)
	}
}
