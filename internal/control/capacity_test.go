package control

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"crypto/tls"
	"crypto/x509"

	"github.com/coder/websocket"

	"github.com/gsoultan/argus/internal/agent"
	"github.com/gsoultan/argus/internal/tunnel"
)

/*
How many browser terminals one control plane relays to agents, and what the
person at each one feels.

Not a pass/fail test: it prints the table published in docs/capacity.md,
and runs only when asked.

	ARGUS_TEST_DATABASE_URL=postgres://… ARGUS_CAPACITY=1 \
	  go test ./internal/control/ -run CapacityOfTheAgentPath -v -timeout 30m

	ARGUS_CAPACITY_SESSIONS=100,250,500  how many at once, one run each
	ARGUS_CAPACITY_HOSTS=50              agents the sessions are spread over
	ARGUS_CAPACITY_TRANSPORT=quic|tcp    which transport the agents hold
	ARGUS_CAPACITY_HOLD=20s              how long each session stays open

Every session is the whole thing: a ticket, the WebSocket, a grant the agent
verifies, a real login shell on a pty, the recording opened before the shell
runs, keystrokes typed and their echo timed, `exit`, and a sealed chain head
in the "closed" message. A session that does not end with a chain head is
counted as failed, whatever else it did.

The agents run in a child process (this test binary again), so the heap and
goroutine figures are the control plane's -- plus the load generator's own
WebSocket clients, which are a small and constant share. The shells are the
host's cost, not the control plane's, and are not in them.
*/

func TestCapacityOfTheAgentPath(t *testing.T) {
	if os.Getenv("ARGUS_CAPACITY") == "" {
		t.Skip("set ARGUS_CAPACITY=1 to measure")
	}
	e := newTunnelEnv(t)
	signer := e.withTerminal(t)
	transport := tunnel.Transport(envOr("ARGUS_CAPACITY_TRANSPORT", "quic"))
	if transport == tunnel.TransportQUIC {
		e.withQUIC(t)
	}
	hosts := envInt(t, "ARGUS_CAPACITY_HOSTS", 50)
	hold, err := time.ParseDuration(envOr("ARGUS_CAPACITY_HOLD", "20s"))
	if err != nil {
		t.Fatal(err)
	}

	// Enroll the hosts and start their agents in a child process.
	var spec capacityAgents
	spec.ControlURL = e.srv.URL
	spec.CA = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE",
		Bytes: e.srv.Certificate().Raw}))
	prefix := unique("cap")
	for i := range hosts {
		host := fmt.Sprintf("%s-%03d", prefix, i)
		cred, _ := enrolled(t, e.store, e.h, host)
		spec.Hosts = append(spec.Hosts, capacityHost{Host: host, Cred: cred})
	}
	runCapacityAgents(t, spec)
	for _, h := range spec.Hosts {
		e.waitTunnel(t, h.Host, transport)
	}

	fmt.Printf("\n%d agents on %s, %s hold per session, GOMAXPROCS=%d\n",
		hosts, transport, hold, runtime.GOMAXPROCS(0))
	fmt.Printf("%-6s %-9s %-15s %-23s %-23s %-11s %-9s %-10s\n", "sess", "ok",
		"open p50/p95", "keystroke p50/p95/p99", "…while 8 run bulk", "bulk MB/s",
		"heap MB", "goroutines")

	for _, n := range envInts(t, "ARGUS_CAPACITY_SESSIONS", []int{100, 250, 500}) {
		r := capacityRun(t, e, signer.IssueTicket, spec.Hosts, n, hold)
		fmt.Printf("%-6d %-9s %-15s %-23s %-23s %-11s %-9.0f %-10d\n", n,
			fmt.Sprintf("%d/%d", r.ok, n),
			fmt.Sprintf("%s/%s", ms(pct(r.open, 50)), ms(pct(r.open, 95))),
			fmt.Sprintf("%s/%s/%s", ms(pct(r.keys, 50)), ms(pct(r.keys, 95)), ms(pct(r.keys, 99))),
			fmt.Sprintf("%s/%s/%s", ms(pct(r.keysBulk, 50)), ms(pct(r.keysBulk, 95)),
				ms(pct(r.keysBulk, 99))),
			fmt.Sprintf("%.1f", r.bulkMBs), r.heapMB, r.goroutines)
		for _, why := range r.failures {
			fmt.Println("   failed:", why)
		}
		// Let the shells and streams of this run go before the next.
		time.Sleep(3 * time.Second)
	}
}

type capacityResult struct {
	ok                   int
	open, keys, keysBulk []time.Duration
	bulkMBs              float64
	heapMB               float64
	goroutines           int
	failures             []string
}

type ticketIssuer func(email, target, principal string, ttl time.Duration) (string, error)

func capacityRun(t *testing.T, e *tunnelEnv, issue ticketIssuer, hosts []capacityHost,
	n int, hold time.Duration) capacityResult {

	var (
		res      capacityResult
		mu       sync.Mutex
		wg       sync.WaitGroup
		opened   sync.WaitGroup
		bulkGo   = make(chan struct{})
		bulkDone sync.WaitGroup
		bulkB    atomic.Int64
	)
	fail := func(why string) {
		mu.Lock()
		defer mu.Unlock()
		if len(res.failures) < 5 {
			res.failures = append(res.failures, why)
		}
	}
	// Eight sessions print in bulk, once there are enough left to type.
	bulkers := 0
	if n >= 16 {
		bulkers = 8
	}
	opened.Add(n)
	bulkDone.Add(bulkers)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			host := hosts[i%len(hosts)].Host
			s, err := openCapacitySession(e, issue, host)
			opened.Done()
			if err != nil {
				fail(err.Error())
				if i < bulkers {
					bulkDone.Done()
				}
				return
			}
			defer s.c.CloseNow()
			mu.Lock()
			res.open = append(res.open, s.openTook)
			mu.Unlock()

			// Keystrokes every 250 ms for the first half of the hold, then
			// the same while eight sessions print as fast as they can.
			if i < bulkers {
				<-bulkGo
				b, err := s.bulk(16 << 20)
				bulkB.Add(b)
				bulkDone.Done()
				if err != nil {
					fail("bulk: " + err.Error())
					return
				}
			} else {
				keys := s.type_(hold/2, 250*time.Millisecond)
				<-bulkGo
				underBulk := s.type_(hold/2, 250*time.Millisecond)
				mu.Lock()
				res.keys = append(res.keys, keys...)
				res.keysBulk = append(res.keysBulk, underBulk...)
				mu.Unlock()
			}
			if err := s.exit(); err != nil {
				fail(err.Error())
				return
			}
			mu.Lock()
			res.ok++
			mu.Unlock()
		}()
	}
	opened.Wait()
	time.Sleep(hold / 2)

	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	res.heapMB = float64(m.HeapInuse) / (1 << 20)
	res.goroutines = runtime.NumGoroutine()

	start := time.Now()
	close(bulkGo)
	bulkDone.Wait()
	if bulkers > 0 {
		res.bulkMBs = float64(bulkB.Load()) / (1 << 20) / time.Since(start).Seconds()
	}
	wg.Wait()
	return res
}

/* ── One browser terminal ────────────────────────────────────────────────── */

type capacitySession struct {
	c        *websocket.Conn
	openTook time.Duration

	out    chan []byte
	closed chan terminalMessage
	errs   chan string
}

func openCapacitySession(e *tunnelEnv, issue ticketIssuer, host string) (*capacitySession, error) {
	start := time.Now()
	ticket, err := issue("load@example.com", host, currentUser(), time.Minute)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, e.terminalURL(host, currentUser(), ticket),
		&websocket.DialOptions{HTTPClient: e.srv.Client(),
			HTTPHeader: http.Header{"Origin": {e.srv.URL}}})
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	c.SetReadLimit(1 << 20)
	s := &capacitySession{c: c, out: make(chan []byte, 1024),
		closed: make(chan terminalMessage, 1), errs: make(chan string, 4)}
	ready := make(chan struct{})
	go func() {
		var once sync.Once
		for {
			typ, data, err := c.Read(context.Background())
			if err != nil {
				return
			}
			if typ == websocket.MessageBinary {
				select {
				case s.out <- data:
				default: // a reader that is not looking is not timing
				}
				continue
			}
			var m terminalMessage
			_ = json.Unmarshal(data, &m)
			switch m.Type {
			case "ready":
				once.Do(func() { close(ready) })
			case "closed":
				s.closed <- m
				return
			case "error":
				select {
				case s.errs <- m.Data:
				default:
				}
			}
		}
	}()
	select {
	case <-ready:
	case why := <-s.errs:
		c.CloseNow()
		return nil, fmt.Errorf("refused: %s", why)
	case <-ctx.Done():
		c.CloseNow()
		return nil, fmt.Errorf("no ready within 30s")
	}
	s.openTook = time.Since(start)
	// The login prints a prompt; stop echoing so a keystroke's answer is
	// the one byte the test sends back, and wait for the shell to settle.
	_ = s.input("stty -echo; PS1=''; echo SET''TLED\n")
	if !s.waitFor("SETTLED", 20*time.Second) {
		c.CloseNow()
		return nil, fmt.Errorf("the shell never answered")
	}
	return s, nil
}

func (s *capacitySession) input(text string) error {
	b, _ := json.Marshal(terminalMessage{Type: "input", Data: text})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.c.Write(ctx, websocket.MessageText, b)
}

func (s *capacitySession) waitFor(marker string, within time.Duration) bool {
	var seen bytes.Buffer
	deadline := time.After(within)
	for {
		select {
		case b := <-s.out:
			seen.Write(b)
			if bytes.Contains(seen.Bytes(), []byte(marker)) {
				return true
			}
			if seen.Len() > 1<<16 {
				seen.Next(seen.Len() - len(marker))
			}
		case <-deadline:
			return false
		}
	}
}

// type_ sends a key every gap for d and times each until output answers it.
// With echo off and no prompt, the only answer is the shell running the
// command, so it has crossed browser, control plane, tunnel, agent, pty and
// shell, and back.
func (s *capacitySession) type_(d, gap time.Duration) []time.Duration {
	var rtts []time.Duration
	for end := time.Now().Add(d); time.Now().Before(end); {
		start := time.Now()
		if s.input("echo k\n") != nil {
			return rtts
		}
		if !s.waitFor("k", 10*time.Second) {
			return rtts
		}
		rtts = append(rtts, time.Since(start))
		time.Sleep(gap)
	}
	return rtts
}

// bulk prints size bytes of base64 and returns how many output bytes came.
func (s *capacitySession) bulk(size int) (int64, error) {
	if err := s.input(fmt.Sprintf("head -c %d /dev/zero | base64 -w 76; echo BULK''DONE\n",
		size*3/4)); err != nil {
		return 0, err
	}
	var got int64
	var tail []byte
	deadline := time.After(5 * time.Minute)
	for {
		select {
		case b := <-s.out:
			got += int64(len(b))
			tail = append(tail, b...)
			if bytes.Contains(tail, []byte("BULKDONE")) {
				return got, nil
			}
			if len(tail) > 64 {
				tail = append([]byte(nil), tail[len(tail)-16:]...)
			}
		case <-deadline:
			return got, fmt.Errorf("bulk output did not finish")
		}
	}
}

func (s *capacitySession) exit() error {
	if err := s.input("exit\n"); err != nil {
		return err
	}
	select {
	case m := <-s.closed:
		if m.ChainHead == "" {
			return fmt.Errorf("closed without a chain head")
		}
		return nil
	case why := <-s.errs:
		return fmt.Errorf("error: %s", why)
	case <-time.After(30 * time.Second):
		return fmt.Errorf("no closed message within 30s")
	}
}

/* ── The agents, in a process of their own ───────────────────────────────── */

type capacityHost struct{ Host, Cred string }

type capacityAgents struct {
	ControlURL string
	CA         string
	Hosts      []capacityHost
}

func runCapacityAgents(t *testing.T, spec capacityAgents) {
	t.Helper()
	b, _ := json.Marshal(spec)
	cmd := exec.Command(os.Args[0], "-test.run=^TestCapacityAgentProcess$", "-test.timeout=0")
	cmd.Env = append(os.Environ(), "ARGUS_CAPACITY_AGENTS="+string(b))
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
}

// TestCapacityAgentProcess is the child: every host's agent, until killed.
func TestCapacityAgentProcess(t *testing.T) {
	raw := os.Getenv("ARGUS_CAPACITY_AGENTS")
	if raw == "" {
		t.Skip("run by TestCapacityOfTheAgentPath")
	}
	var spec capacityAgents
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(spec.CA))
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	for _, h := range spec.Hosts {
		col := agent.NewCollector(dir+"/"+h.Host, log)
		services := agent.DefaultTunnelServices()
		services[agent.TerminalService] = agent.Terminal(agent.TerminalConfig{
			Collector: col, Verifier: &tunnel.GrantVerifier{Host: h.Host},
			Hostname: h.Host, Log: log, Spawn: agent.SpawnDirect,
		})
		go agent.RunTunnel(context.Background(), agent.TunnelConfig{
			Dial: tunnel.DialOptions{ControlURL: spec.ControlURL, Credential: h.Cred,
				Hostname: h.Host, Version: "capacity",
				TLS: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
			Services: services, Log: log,
		})
	}
	select {}
}

/* ── Small things ────────────────────────────────────────────────────────── */

func currentUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "root"
}

func pct(d []time.Duration, p int) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[min(len(s)-1, len(s)*p/100)]
}

func ms(d time.Duration) string {
	return strconv.FormatFloat(float64(d.Microseconds())/1000, 'f', 1, 64)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(t *testing.T, k string, def int) int {
	v, err := strconv.Atoi(envOr(k, strconv.Itoa(def)))
	if err != nil {
		t.Fatalf("%s: %v", k, err)
	}
	return v
}

func envInts(t *testing.T, k string, def []int) []int {
	raw := os.Getenv(k)
	if raw == "" {
		return def
	}
	var out []int
	for _, f := range strings.Split(raw, ",") {
		v, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		out = append(out, v)
	}
	return out
}
