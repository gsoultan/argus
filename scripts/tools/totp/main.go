// Command totp prints the current authenticator code for a secret.
//
// For the install drills: a privileged account must have a second factor
// before the console lets it do anything else, so the drill enrols one the
// way a person would (password, scan, code) and needs an app's code to do it.
//
//	totp JBSWY3DPEHPK3PXP
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/gsoultan/argus/internal/auth"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: totp <base32 secret>")
		os.Exit(2)
	}
	code, err := auth.TOTPCode(os.Args[1], time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(code)
}
