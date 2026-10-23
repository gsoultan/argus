package tunnel

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

/*
What one tunnel carries, on each transport, at a given round trip.

Not a pass/fail test: it prints the table published in docs/capacity.md,
and runs only when asked, because the numbers depend on
the machine and take a while.

	ARGUS_TUNNEL_CAPACITY=1 go test ./internal/tunnel/ -run Capacity -v

Both ends are the real ones: DialUpgrade and DialQUIC with the configurations
the agent and the control plane use. Between them is a proxy that delays every
byte by half the round trip in each direction and nothing else -- no loss, no
bandwidth cap -- so what limits a stream is the transport's own window, which
is the thing being measured. Loopback with no added delay is the CPU ceiling.

Two shapes, because they are what sessions do:

  - bulk: the agent writes terminal-output frames (32 KB) as fast as the
    stream takes them, the way `cat` of a large file or a download does.
  - echo: one six-byte frame each way, the way a keystroke and its echo go,
    alone and then while a bulk stream runs on the same tunnel. The second is
    the one that matters: one person's large output must not make everyone
    else's typing on that host lag.
*/

func TestCapacityOfOneTunnel(t *testing.T) {
	if os.Getenv("ARGUS_TUNNEL_CAPACITY") == "" {
		t.Skip("set ARGUS_TUNNEL_CAPACITY=1 to measure")
	}
	const bulkBytes = 64 << 20
	fmt.Printf("\n%-5s %-6s %12s %12s %10s %10s %14s\n",
		"rtt", "trans", "1 stream", "8 streams", "echo p50", "echo p99", "echo p99 bulk")
	for _, rtt := range []time.Duration{0, 20 * time.Millisecond, 80 * time.Millisecond} {
		for _, tr := range []Transport{TransportUpgrade, TransportQUIC} {
			ctl, _ := capacityPair(t, tr, rtt)
			size := int64(bulkBytes)
			if rtt > 0 {
				size = 16 << 20
			}
			one := bulk(t, ctl, 1, size)
			eight := bulk(t, ctl, 8, size/2)
			p50, p99 := echo(t, ctl, 200, false)
			_, p99b := echo(t, ctl, 100, true)
			fmt.Printf("%-5s %-6s %9.1f MB/s %9.1f MB/s %10s %10s %14s\n",
				rtt, tr, one, eight, p50.Round(10*time.Microsecond),
				p99.Round(10*time.Microsecond), p99b.Round(100*time.Microsecond))
			_ = ctl.Close()
		}
	}
}

/* ── Measurements ────────────────────────────────────────────────────────── */

// bulk opens streams on ctl, each of which the agent fills with size bytes
// of terminal-output frames, and returns the aggregate rate in MB/s.
func bulk(t *testing.T, ctl Session, streams int, size int64) float64 {
	t.Helper()
	start := time.Now()
	var wg sync.WaitGroup
	for range streams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := Open(context.Background(), ctl, fmt.Sprintf("bulk:%d", size))
			if err != nil {
				t.Error(err)
				return
			}
			defer st.Close()
			fr := NewFrameReader(st)
			var got int64
			for got < size {
				_, p, err := fr.Next()
				if err != nil {
					t.Error(err)
					return
				}
				got += int64(len(p))
			}
		}()
	}
	wg.Wait()
	return float64(size*int64(streams)) / (1 << 20) / time.Since(start).Seconds()
}

// echo times n keystroke round trips, optionally while a bulk stream runs
// on the same tunnel.
func echo(t *testing.T, ctl Session, n int, underBulk bool) (p50, p99 time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if underBulk {
		bst, err := Open(ctx, ctl, "bulk:0")
		if err != nil {
			t.Fatal(err)
		}
		defer bst.Close()
		go func() { _, _ = io.Copy(io.Discard, bst) }()
		time.Sleep(200 * time.Millisecond) // let it reach its window
	}
	st, err := Open(ctx, ctl, "echo")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fr := NewFrameReader(st)
	rtts := make([]time.Duration, 0, n)
	for range n {
		start := time.Now()
		if err := WriteFrame(st, FrameInput, []byte("x")); err != nil {
			t.Fatal(err)
		}
		if _, _, err := fr.Next(); err != nil {
			t.Fatal(err)
		}
		rtts = append(rtts, time.Since(start))
	}
	sort.Slice(rtts, func(i, j int) bool { return rtts[i] < rtts[j] })
	return rtts[n/2], rtts[n*99/100]
}

// serveCapacity is the agent's side: "bulk:N" writes N bytes of output
// frames (forever when N is 0), "echo" answers every frame with one.
func serveCapacity(sess Session) {
	for {
		st, err := sess.AcceptStream(context.Background())
		if err != nil {
			return
		}
		go func() {
			defer st.Close()
			var h Header
			if ReadJSON(st, &h) != nil || WriteJSON(st, Answer{OK: true}) != nil {
				return
			}
			_ = st.SetDeadline(time.Time{})
			switch {
			case strings.HasPrefix(h.Service, "bulk:"):
				var size int64
				_, _ = fmt.Sscanf(h.Service, "bulk:%d", &size)
				buf := make([]byte, 32<<10)
				for sent := int64(0); size == 0 || sent < size; sent += int64(len(buf)) {
					if WriteFrame(st, FrameOutput, buf) != nil {
						return
					}
				}
			case h.Service == "echo":
				fr := NewFrameReader(st)
				for {
					_, p, err := fr.Next()
					if err != nil || WriteFrame(st, FrameOutput, p) != nil {
						return
					}
				}
			}
		}()
	}
}

/* ── A tunnel through a delaying proxy ───────────────────────────────────── */

// capacityPair returns the control plane's and the agent's ends of one
// tunnel on transport tr, with rtt added between them.
func capacityPair(t *testing.T, tr Transport, rtt time.Duration) (ctl, agent Session) {
	t.Helper()
	got := make(chan Session, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\n" +
			"Upgrade: " + UpgradeToken + "\r\n\r\n")
		_ = buf.Flush()
		s, err := NewServerMux(conn, buf.Reader)
		if err == nil {
			got <- s
		}
	}))
	srv.EnableHTTP2 = false
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	o := DialOptions{Credential: "c", Hostname: "h", TLS: &tls.Config{RootCAs: pool}}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	switch tr {
	case TransportUpgrade:
		front := delayTCP(t, srv.Listener.Addr().String(), rtt/2)
		o.ControlURL = "https://" + front
		a, _, err := DialUpgrade(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		agent = a
		ctl = <-got
	case TransportQUIC:
		qtls := srv.TLS.Clone()
		qtls.NextProtos = []string{ALPN}
		ln, err := quic.ListenAddr("127.0.0.1:0", qtls, ServerQUICConfig())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		go func() {
			c, err := ln.Accept(context.Background())
			if err != nil {
				return
			}
			s := NewQUICSession(c)
			st, err := s.AcceptStream(context.Background())
			if err != nil {
				return
			}
			var h Hello
			_ = ReadJSON(st, &h)
			_ = WriteJSON(st, Answer{OK: true})
			_ = st.Close()
			got <- s
		}()
		front := delayUDP(t, ln.Addr().String(), rtt/2)
		o.ControlURL = "https://127.0.0.1"
		_, port, _ := net.SplitHostPort(front)
		var p int
		_, _ = fmt.Sscanf(port, "%d", &p)
		a, err := DialQUIC(ctx, o, p)
		if err != nil {
			t.Fatal(err)
		}
		agent = a
		ctl = <-got
	}
	t.Cleanup(func() { _ = agent.Close() })
	go serveCapacity(agent)
	return ctl, agent
}

type delayed struct {
	due time.Time
	b   []byte
}

// pump copies src to dst, holding every chunk for d.
func pump(dst io.Writer, src io.Reader, d time.Duration, done func()) {
	q := make(chan delayed, 1<<14)
	go func() {
		defer done()
		for c := range q {
			time.Sleep(time.Until(c.due))
			if _, err := dst.Write(c.b); err != nil {
				return
			}
		}
	}()
	for {
		b := make([]byte, 64<<10)
		n, err := src.Read(b)
		if n > 0 {
			q <- delayed{time.Now().Add(d), b[:n]}
		}
		if err != nil {
			close(q)
			return
		}
	}
}

func delayTCP(t *testing.T, upstream string, d time.Duration) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			u, err := net.Dial("tcp", upstream)
			if err != nil {
				_ = c.Close()
				continue
			}
			closeBoth := func() { _ = c.Close(); _ = u.Close() }
			go pump(u, c, d, closeBoth)
			go pump(c, u, d, closeBoth)
		}
	}()
	return ln.Addr().String()
}

// delayUDP forwards datagrams for one client to upstream, holding each for d.
func delayUDP(t *testing.T, upstream string, d time.Duration) string {
	t.Helper()
	front, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	up, err := net.Dial("udp", upstream)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = front.Close(); _ = up.Close() })
	var (
		mu     sync.Mutex
		client net.Addr
	)
	send := func(write func([]byte) error) chan<- delayed {
		q := make(chan delayed, 1<<14)
		go func() {
			for c := range q {
				time.Sleep(time.Until(c.due))
				if write(c.b) != nil {
					return
				}
			}
		}()
		return q
	}
	toUp := send(func(b []byte) error { _, err := up.Write(b); return err })
	toClient := send(func(b []byte) error {
		mu.Lock()
		a := client
		mu.Unlock()
		_, err := front.WriteTo(b, a)
		return err
	})
	go func() {
		for {
			b := make([]byte, 2048)
			n, a, err := front.ReadFrom(b)
			if err != nil {
				return
			}
			mu.Lock()
			client = a
			mu.Unlock()
			toUp <- delayed{time.Now().Add(d), b[:n]}
		}
	}()
	go func() {
		for {
			b := make([]byte, 2048)
			n, err := up.Read(b)
			if err != nil {
				return
			}
			toClient <- delayed{time.Now().Add(d), b[:n]}
		}
	}()
	return front.LocalAddr().String()
}
