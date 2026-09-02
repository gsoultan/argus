package recorder

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
)

// Verification is the result of recomputing a recording's hash chain.
type Verification struct {
	OK bool
	// Head is the recomputed chain head. When OK, it equals the value the
	// gateway published at close.
	Head string
	// Lines checked before stopping.
	Lines int
	// BrokenAt is the 1-indexed line where the recomputed chain diverged from
	// the expected head, or 0 when the chain is intact.
	BrokenAt int
}

// Verify recomputes the chain over r and compares it to expectedHead.
//
// This deliberately re-derives everything from the bytes on disk rather than
// trusting any sidecar metadata: the point of the chain is that an auditor can
// check it without trusting the system that produced it. The browser console
// runs the identical computation in a Web Worker, so a recording verified here
// verifies there too.
//
// Passing an empty expectedHead computes the head without comparing.
func Verify(r io.Reader, expectedHead string) (Verification, error) {
	sc := bufio.NewScanner(r)
	// Terminal output arrives in bursts; a single line can carry a screenful of
	// a `find /` or a base64 blob. The default 64 KB token limit is too small.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	head := Genesis
	lines := 0

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		sum := sha256.New()
		sum.Write([]byte(head))
		sum.Write(line)
		head = hex.EncodeToString(sum.Sum(nil))
		lines++
	}
	if err := sc.Err(); err != nil {
		return Verification{Head: head, Lines: lines}, fmt.Errorf("read recording: %w", err)
	}

	v := Verification{Head: head, Lines: lines, OK: true}
	if expectedHead != "" && head != expectedHead {
		// The chain is self-consistent by construction, so a mismatch means the
		// file differs from what was recorded — content changed, truncated, or
		// appended to. We cannot say which line without the original hashes, so
		// report the whole artefact as suspect rather than guess.
		v.OK = false
		v.BrokenAt = -1
	}
	return v, nil
}
