// Command argus-verify checks the integrity of a session recording.
//
// It recomputes the hash chain from the bytes on disk and compares it to the
// chain head the gateway published when it sealed the session. Nothing else is
// consulted — that is the point. An auditor can verify a recording without
// trusting Argus, the storage it came from, or the person handing it over.
//
//	argus-verify session.cast <chain-head>
//	argus-verify --head session.cast          # just print the computed head
//	argus-verify --cat session.cast.zst       # print the recording itself
//
// Recordings are kept compressed (zstd, and gzip on hosts an older agent
// tidied); every mode reads through the compression.
//
// It also opens an encrypted database backup (.argusdump) outside Argus, for
// break-glass recovery when the control plane is gone:
//
//	argus-verify backup.argusdump                      # print the header
//	argus-verify --secret <root secret> backup.argusdump --out plain.dump
//	argus-verify --key <data key> backup.argusdump --out plain.dump
//
// The root secret is the control plane's grant key seed, as hex; the data
// key is the one a restore grant carries. The output is a pg_dump custom
// archive for pg_restore. Every frame is authenticated before it is written,
// and a file that fails to open is reported, never half written.
//
// Exit status is 0 when the recording is intact and 1 when it is not, so this
// drops into a compliance pipeline directly.
package main

import (
	"bufio"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/gsoultan/argus/internal/recorder"
	"github.com/gsoultan/argus/internal/tunnel"
)

// Stamped by the release build.
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	headOnly := flag.Bool("head", false, "print the computed chain head and exit")
	cat := flag.Bool("cat", false, "print the recording, decompressed, and exit; nothing is verified")
	quiet := flag.Bool("quiet", false, "suppress output; rely on exit status")
	secret := flag.String("secret", "", "the control plane's root secret, to open a backup")
	dataKey := flag.String("key", "", "a backup's data key (base64), to open it without the secret")
	out := flag.String("out", "", "where a decrypted backup goes (default: stdout)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: argus-verify [--head] [--quiet] <recording.cast> [expected-head]\n"+
			"       argus-verify [--secret S | --key K] [--out plain.dump] <backup.argusdump>\n")
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
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "argus-verify: %v\n", err)
		os.Exit(2)
	}
	defer f.Close()

	if peek, _ := bufio.NewReader(f).Peek(16); tunnel.IsDumpFile(peek) {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			fmt.Fprintf(os.Stderr, "argus-verify: %v\n", err)
			os.Exit(2)
		}
		os.Exit(openBackup(f, *secret, *dataKey, *out, *quiet))
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		fmt.Fprintf(os.Stderr, "argus-verify: %v\n", err)
		os.Exit(2)
	}
	if *cat {
		plain, err := recorder.Open(f)
		if err == nil {
			_, err = io.Copy(os.Stdout, plain)
			_ = plain.Close()
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "argus-verify: %v\n", err)
			os.Exit(2)
		}
		return
	}
	if !*headOnly && expected == "" {
		fmt.Fprintln(os.Stderr, "argus-verify: an expected chain head is required (or pass --head)")
		os.Exit(2)
	}

	// Recordings are kept compressed: zstd once sealed, and gzip on a host
	// an older agent tidied. The chain covers the recording's lines, not the
	// file's bytes, so a compressed copy is read through and checked exactly
	// as it was written.
	plain, err := recorder.Open(f)
	if err != nil {
		fmt.Fprintf(os.Stderr, "argus-verify: %v\n", err)
		os.Exit(2)
	}
	defer plain.Close()
	br := bufio.NewReader(plain)

	// A database session's recording (.argussql) is JSON lines with its own
	// chain; the first bytes say which kind this is.
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

// openBackup prints a backup's header and, given a secret or a data key,
// decrypts it. The exit status is 0 when every frame opened, 1 when the file
// is not what it says it is, 2 for a usage or I/O problem.
func openBackup(f *os.File, secret, dataKey, out string, quiet bool) int {
	br := bufio.NewReaderSize(f, 1<<20)
	h, _, err := tunnel.ReadDumpHeader(br)
	if err != nil {
		fmt.Fprintf(os.Stderr, "argus-verify: %v\n", err)
		return 1
	}
	if !quiet {
		fmt.Printf("BACKUP   %s\n", f.Name())
		fmt.Printf("  id        %s\n", h.BackupID)
		fmt.Printf("  database  %s on %s (org %s)\n", h.DBName, h.Host, h.Organization)
		fmt.Printf("  taken     %s\n", h.StartedAt.Format("2006-01-02 15:04:05 MST"))
		fmt.Printf("  server    %s, pg_dump %s, %s\n", h.ServerVersion, h.DumpVersion, h.Compression)
		fmt.Printf("  key       %s\n", h.KeyID)
		if h.Reason != "" {
			fmt.Printf("  reason    %s\n", h.Reason)
		}
	}
	if secret == "" && dataKey == "" {
		if !quiet {
			fmt.Println("  pass --secret or --key to decrypt it")
		}
		return 0
	}
	var key []byte
	if dataKey != "" {
		key, err = base64.StdEncoding.DecodeString(dataKey)
		if err != nil {
			fmt.Fprintln(os.Stderr, "argus-verify: the data key is not base64")
			return 2
		}
	} else {
		priv, derr := tunnel.DeriveBackupKey(secret, h.Organization)
		if derr != nil {
			fmt.Fprintf(os.Stderr, "argus-verify: %v\n", derr)
			return 2
		}
		if got := tunnel.BackupKeyID(priv.PublicKey()); got != h.KeyID {
			fmt.Fprintf(os.Stderr, "argus-verify: this secret derives key %s, and the backup was made for %s\n", got, h.KeyID)
			return 1
		}
		key, err = tunnel.UnwrapBackupKey(priv, h)
		if err != nil {
			fmt.Fprintf(os.Stderr, "argus-verify: %v\n", err)
			return 1
		}
	}
	// Re-read from the start: the reader checks the header itself.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		fmt.Fprintf(os.Stderr, "argus-verify: %v\n", err)
		return 2
	}
	dr, err := tunnel.NewDumpReader(bufio.NewReaderSize(f, 1<<20), key, h.BackupID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "argus-verify: %v\n", err)
		return 1
	}
	var w io.Writer = os.Stdout
	var of *os.File
	if out != "" {
		of, err = os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Fprintf(os.Stderr, "argus-verify: %v\n", err)
			return 2
		}
		w = of
	}
	n, err := io.Copy(w, dr)
	if of != nil {
		// Closed before any remove: Windows refuses to delete an open file.
		if cerr := of.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "argus-verify: %v (after %d bytes; the output is incomplete)\n", err, n)
		if out != "" {
			_ = os.Remove(out)
		}
		return 1
	}
	if !quiet {
		fmt.Fprintf(os.Stderr, "  wrote     %d bytes of pg_dump archive; restore with pg_restore -d <db> %s\n", n, out)
	}
	return 0
}
