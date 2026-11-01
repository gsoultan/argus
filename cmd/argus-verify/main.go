// Command argus-verify checks the integrity of a session recording.
//
// It recomputes the hash chain from the bytes on disk and compares it to the
// chain head the gateway published when it sealed the session. Nothing else is
// consulted — that is the point. An auditor can verify a recording without
// trusting Argus, the storage it came from, or the person handing it over.
//
//	argus-verify session.cast <chain-head>
//	argus-verify --head session.cast          # just print the computed head
//
// Exit status is 0 when the recording is intact and 1 when it is not, so this
// drops into a compliance pipeline directly.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"

	"github.com/gsoultan/argus/internal/recorder"
	"github.com/gsoultan/argus/internal/tunnel"
)

// Stamped by the release build.
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	headOnly := flag.Bool("head", false, "print the computed chain head and exit")
	quiet := flag.Bool("quiet", false, "suppress output; rely on exit status")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: argus-verify [--head] [--quiet] <recording.cast> [expected-head]\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Printf("argus-verify %s\n", version)
		return
	}

	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(2)
	}
	path := flag.Arg(0)
	expected := ""
	if flag.NArg() > 1 {
		expected = flag.Arg(1)
	}
	if !*headOnly && expected == "" {
		fmt.Fprintln(os.Stderr, "argus-verify: an expected chain head is required (or pass --head)")
		os.Exit(2)
	}

	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "argus-verify: %v\n", err)
		os.Exit(2)
	}
	defer f.Close()

	// A database session's recording (.argussql) is JSON lines with its own
	// chain; the first bytes say which kind this is.
	br := bufio.NewReader(f)
	peek, _ := br.Peek(16)
	var v recorder.Verification
	var broken int64
	if tunnel.IsPGRecording(peek) {
		pv, perr := tunnel.VerifyPGRecording(br, expected)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "argus-verify: %v\n", perr)
			os.Exit(2)
		}
		v = recorder.Verification{OK: pv.OK, Lines: pv.Lines, Head: pv.Head}
		broken = pv.Broken
	} else {
		v, err = recorder.Verify(br, expected)
		if err != nil {
			fmt.Fprintf(os.Stderr, "argus-verify: %v\n", err)
			os.Exit(2)
		}
	}

	if *headOnly {
		fmt.Println(v.Head)
		return
	}

	if v.OK {
		if !*quiet {
			fmt.Printf("INTACT   %s\n", path)
			fmt.Printf("  lines  %d\n", v.Lines)
			fmt.Printf("  head   %s\n", v.Head)
		}
		return
	}

	if !*quiet {
		fmt.Printf("TAMPERED %s\n", path)
		fmt.Printf("  lines     %d\n", v.Lines)
		fmt.Printf("  expected  %s\n", expected)
		fmt.Printf("  computed  %s\n", v.Head)
		if broken > 0 {
			// A database recording's lines each carry their own hash, so a
			// changed line is found exactly, not only by the head.
			fmt.Printf("  broken    line %d does not match its own hash or the one before it\n", broken)
		}
		fmt.Println()
		fmt.Println("  The recording on disk differs from what was sealed.")
		fmt.Println("  Preserve it as evidence and treat this as an incident.")
	}
	os.Exit(1)
}
