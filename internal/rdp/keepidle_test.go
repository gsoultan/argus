package rdp

import (
	"errors"
	"net"
	"testing"
	"time"
)

// idleClient is a live session over a pipe whose far end is the test.
func idleClient(t *testing.T, keepIdle bool) (*Client, net.Conn) {
	t.Helper()
	near, far := net.Pipe()
	t.Cleanup(func() { _ = near.Close(); _ = far.Close() })
	c := &Client{conn: near, rea: NewReassembler(), readTimeout: 40 * time.Millisecond,
		keepIdle: keepIdle, shareID: 0x103ea, userID: 1007, channel: 1003, width: 1024, height: 768}
	return c, far
}

// drain reads whatever the client writes, reporting each write.
func drain(far net.Conn) <-chan int {
	got := make(chan int, 8)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := far.Read(buf)
			if err != nil {
				close(got)
				return
			}
			got <- n
		}
	}()
	return got
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// Windows sends nothing while the screen does not change. Without KeepIdle the
// first quiet read timeout ended the session: that is what cut a Remote
// Desktop off two minutes after the person stopped moving the mouse.
func TestQuietScreenEndsSessionWithoutKeepIdle(t *testing.T) {
	c, far := idleClient(t, false)
	drain(far)
	if _, err := c.NextBitmaps(); !isTimeout(err) {
		t.Fatalf("NextBitmaps = %v, want the timeout", err)
	}
}

func TestQuietScreenAsksForRepaintAndKeepsGoing(t *testing.T) {
	c, far := idleClient(t, true)
	writes := drain(far)

	if b, err := c.NextBitmaps(); err != nil || b != nil {
		t.Fatalf("first quiet read = %v, %v; want nothing and no error", b, err)
	}
	select {
	case n := <-writes:
		if n == 0 {
			t.Fatal("empty repaint request")
		}
	case <-time.After(time.Second):
		t.Fatal("no repaint was asked for after a quiet read")
	}

	// The server answers: the probe is spent and the next quiet spell gets
	// one of its own.
	go func() { _, _ = far.Write([]byte{0x00, 0x02}) }()
	if _, err := c.NextBitmaps(); err != nil {
		t.Fatalf("read after the answer: %v", err)
	}
	if c.probed {
		t.Fatal("an answer did not reset the probe")
	}
	if _, err := c.NextBitmaps(); err != nil {
		t.Fatalf("second quiet spell after an answer ended the session: %v", err)
	}
}

// A server that does not answer the repaint is dead, not quiet.
func TestSilentAfterRepaintEndsSession(t *testing.T) {
	c, far := idleClient(t, true)
	drain(far)
	if _, err := c.NextBitmaps(); err != nil {
		t.Fatalf("first quiet read: %v", err)
	}
	if _, err := c.NextBitmaps(); !isTimeout(err) {
		t.Fatalf("second quiet read = %v, want the timeout", err)
	}
}

// A timeout part-way through a PDU leaves the stream out of step, so it is
// never taken for a quiet screen.
func TestTimeoutMidPDUIsNotQuiet(t *testing.T) {
	c, far := idleClient(t, true)
	go func() { _, _ = far.Write([]byte{0x03}) }()
	if _, err := c.NextBitmaps(); !isTimeout(err) {
		t.Fatalf("NextBitmaps = %v, want the timeout", err)
	}
}
