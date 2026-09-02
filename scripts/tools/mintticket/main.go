// Command mintticket issues tickets for poking at a running gateway.
//
// Development only. It exists because the gateway verifies signatures and
// holds no policy of its own, so exercising its endpoints by hand needs
// something to stand in for the control plane.
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/gsoultan/argus/internal/auth"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: mintticket shadow|terminate <session-id> | terminal <target> <principal>")
		os.Exit(2)
	}
	s, err := auth.NewSigner(os.Getenv("ARGUS_SIGNING_SECRET"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "signer:", err)
		os.Exit(1)
	}

	var token string
	switch os.Args[1] {
	case "shadow", "terminate":
		token, err = s.IssueSessionScopedTicket("auditor@northwind.id", os.Args[1], os.Args[2], time.Minute)
	case "terminal":
		token, err = s.IssueTicket("dewi.p@northwind.id", os.Args[2], os.Args[3], time.Minute)
	default:
		fmt.Fprintln(os.Stderr, "unknown scope", os.Args[1])
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "issue:", err)
		os.Exit(1)
	}
	fmt.Print(token)
}
