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
	hosts := []string{"localhost", "argus.local", "127.0.0.1", "::1", "host.containers.internal"}
	if err := tlsconfig.GenerateDevPKI(dir, hosts); err != nil {
		fmt.Fprintf(os.Stderr, "gencerts: %v\n", err)
		os.Exit(1)
	}
}
