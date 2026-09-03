// Package hashchain writes newline-delimited records under a tamper-evident
// hash chain.
//
// Each link is SHA-256(prevHash || line), so editing any line invalidates every
// link after it. An auditor can detect tampering with nothing but the file and
// the published head.
//
// Extracted so the SSH and RDP recorders share one implementation rather than
// two. This is the property the product is sold on; a second copy of it would
// be a second place for it to be quietly wrong, and the two would drift the
// first time one of them was fixed. It is also what lets one verifier —
// recorder.Verify, the console's Web Worker, argus-verify and the backup drill
// — check both formats without knowing anything about either.
package hashchain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
)

// Genesis is the chain head before anything has been written.
const Genesis = "0000000000000000000000000000000000000000000000000000000000000000"

// Chain appends lines to a writer while maintaining the hash chain.
//
// Safe for concurrent use: a proxied session writes from more than one
// goroutine, and the lock is what orders the chain.
type Chain struct {
	mu     sync.Mutex
	w      io.Writer
	head   string
	lines  int
	bytes  int64
	closed bool
	tap    func(line []byte)
}

// New returns a Chain positioned at Genesis.
func New(w io.Writer) *Chain {
	return &Chain{w: w, head: Genesis}
}

// SetTap registers a function called with every line as it is committed.
//
// Invoked with the lock held, which is what orders frames for a live viewer, so
// it must not block and must not call back into the Chain. A nil tap removes
// any previous one.
func (c *Chain) SetTap(fn func(line []byte)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tap = fn
}

// Emit appends one line and advances the chain.
//
// The line must not contain a newline; it is the record separator, and one
// embedded in a record would split it into two links that verify independently
// of the record they came from.
func (c *Chain) Emit(line []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.emitLocked(line)
}

func (c *Chain) emitLocked(line []byte) error {
	if c.closed {
		return fmt.Errorf("chain is closed")
	}

	sum := sha256.New()
	sum.Write([]byte(c.head))
	sum.Write(line)
	c.head = hex.EncodeToString(sum.Sum(nil))

	if _, err := c.w.Write(append(line, '\n')); err != nil {
		// Write failures are fatal by design: the caller is expected to
		// terminate the session rather than let it continue unrecorded. An
		// auditor asking "are all privileged sessions recorded?" needs that to
		// be true without an asterisk.
		return fmt.Errorf("write record: %w", err)
	}
	c.lines++
	c.bytes += int64(len(line)) + 1

	// After the write, never before: a record that failed to commit must not be
	// shown to a viewer as though it had. The copy is required because the
	// append above may write into line's spare capacity.
	if c.tap != nil {
		frame := make([]byte, len(line))
		copy(frame, line)
		c.tap(frame)
	}
	return nil
}

// Head returns the current chain head without closing.
func (c *Chain) Head() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.head
}

// Stats reports progress.
func (c *Chain) Stats() (lines int, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lines, c.bytes
}

// Close finalises the chain and returns the head, which the control plane
// stores alongside the session so the artefact can be verified later.
func (c *Chain) Close() (head string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return c.head, nil
	}
	c.closed = true
	if closer, ok := c.w.(io.Closer); ok {
		err = closer.Close()
	}
	return c.head, err
}

// Closed reports whether Close has run.
func (c *Chain) Closed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}
