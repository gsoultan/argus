// Command loadtest opens many concurrent SSH sessions through the gateway and
// reports what actually happened.
//
// It exists because "how many sessions can this hold?" was a number nobody
// here had measured. A PAM gateway that falls over at the point a customer
// needs it most is worse than one with a published, modest limit, and the only
// way to publish a limit honestly is to reach it on purpose.
//
// Every session does the full thing: TCP, SSH handshake, publickey auth, a PTY
// request, a shell, a command whose output is read back and checked, then a
// hold and a clean close. A test that stops after the handshake measures the
// listener, not the product -- recording, relaying and the audit report are
// where the work is.
//
//	go run ./scripts/tools/loadtest -n 200 -hold 10s
//
// Two things have to be moved out of the way first, or the number measured is
// somebody else's limit:
//
//   - the gateway's own connection rate limit (dev/argus.yaml sets 6/min,
//     which is a defence against password spraying, not a capacity ceiling)
//   - the target's sshd, whose default MaxStartups 10:30:100 starts refusing
//     at ten unauthenticated connections. Left in place it caps the result at
//     roughly 76% success and looks exactly like a gateway fault.
//
// Restore both afterwards. The published figures in README.md were taken with
// both raised, against one target, on a single host.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

type result struct {
	dial, handshake, session, firstByte, total time.Duration
	err                                        error
}

func main() {
	var (
		addr      = flag.String("addr", "127.0.0.1:2222", "gateway address")
		keyPath   = flag.String("key", os.Getenv("HOME")+"/.ssh/id_ed25519", "private key")
		principal = flag.String("principal", "ops", "principal to request")
		target    = flag.String("target", "pay-01.payments.northwind.id", "target host")
		n         = flag.Int("n", 200, "concurrent sessions")
		hold      = flag.Duration("hold", 10*time.Second, "how long to hold each session open")
		ramp      = flag.Duration("ramp", 0, "spread session starts over this window (0 = all at once)")
		cmd       = flag.String("cmd", "echo ARGUS-LOADTEST-OK", "command to run in each session")
	)
	flag.Parse()

	signer, err := loadKey(*keyPath)
	if err != nil {
		die("read key: %v", err)
	}
	cfg := &ssh.ClientConfig{
		User:            *principal + ":" + *target,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // measuring the gateway, not trusting it
		Timeout:         30 * time.Second,
	}

	fmt.Printf("opening %d concurrent sessions to %s as %s\n", *n, *addr, cfg.User)
	if *ramp > 0 {
		fmt.Printf("ramping starts over %s\n", *ramp)
	}

	results := make([]result, *n)
	var live, peak int64
	var wg sync.WaitGroup
	start := time.Now()

	for i := 0; i < *n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if *ramp > 0 {
				time.Sleep(time.Duration(int64(*ramp) * int64(i) / int64(*n)))
			}
			cur := atomic.AddInt64(&live, 1)
			for {
				p := atomic.LoadInt64(&peak)
				if cur <= p || atomic.CompareAndSwapInt64(&peak, p, cur) {
					break
				}
			}
			results[i] = one(*addr, cfg, *cmd, *hold)
			atomic.AddInt64(&live, -1)
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	report(results, elapsed, atomic.LoadInt64(&peak))
}

// syncBuffer merges stdout and stderr safely.
//
// x/crypto/ssh runs one copy goroutine per stream, and bytes.Buffer is not safe
// for concurrent use. Pointing both at one bare buffer silently dropped output:
// sessions that had in fact succeeded were scored as failures, at a rate that
// rose with load. A load test that manufactures its own failures is worse than
// no load test, because the numbers look like a finding.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// one runs a single session end to end.
//
// The return is named so the deferred total actually lands in the value the
// caller receives; with an unnamed return the copy happens first and every
// total is zero.
func one(addr string, cfg *ssh.ClientConfig, command string, hold time.Duration) (r result) {
	begin := time.Now()
	defer func() { r.total = time.Since(begin) }()

	t0 := time.Now()
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		r.err = fmt.Errorf("dial: %w", err)
		return r
	}
	defer client.Close()
	// Dial covers TCP and the handshake together; splitting them would need a
	// custom transport and would not change the conclusion.
	r.dial = time.Since(t0)
	r.handshake = r.dial

	t1 := time.Now()
	sess, err := client.NewSession()
	if err != nil {
		r.err = fmt.Errorf("open session: %w", err)
		return r
	}
	defer sess.Close()

	if err := sess.RequestPty("xterm-256color", 24, 80, ssh.TerminalModes{
		ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400,
	}); err != nil {
		r.err = fmt.Errorf("pty: %w", err)
		return r
	}
	r.session = time.Since(t1)

	t2 := time.Now()
	var out syncBuffer
	sess.Stdout = &out
	sess.Stderr = &out
	if err := sess.Start(command); err != nil {
		r.err = fmt.Errorf("start: %w", err)
		return r
	}
	if err := sess.Wait(); err != nil {
		r.err = fmt.Errorf("wait: %w", err)
		return r
	}
	r.firstByte = time.Since(t2)

	// The output has to be right. A session that connects and returns nothing
	// is a failure the timings alone would score as a success.
	if !strings.Contains(out.String(), "ARGUS-LOADTEST-OK") &&
		strings.Contains(command, "ARGUS-LOADTEST-OK") {
		r.err = fmt.Errorf("command produced no expected output: %q", trim(out.String()))
		return r
	}

	time.Sleep(hold)
	return r
}

func report(rs []result, elapsed time.Duration, peak int64) {
	var okr []result
	fails := map[string]int{}
	for _, r := range rs {
		if r.err != nil {
			fails[classify(r.err)]++
			continue
		}
		okr = append(okr, r)
	}

	fmt.Printf("\n%-22s %s\n", "wall clock", elapsed.Round(time.Millisecond))
	fmt.Printf("%-22s %d\n", "peak concurrent", peak)
	fmt.Printf("%-22s %d / %d (%.1f%%)\n", "succeeded", len(okr), len(rs),
		100*float64(len(okr))/float64(len(rs)))

	if len(fails) > 0 {
		fmt.Printf("\nfailures\n")
		keys := make([]string, 0, len(fails))
		for k := range fails {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return fails[keys[i]] > fails[keys[j]] })
		for _, k := range keys {
			fmt.Printf("  %4d  %s\n", fails[k], k)
		}
	}
	if len(okr) == 0 {
		fmt.Println("\nno successful sessions; nothing to summarise")
		os.Exit(1)
	}

	fmt.Printf("\n%-14s %8s %8s %8s %8s\n", "stage", "p50", "p95", "p99", "max")
	row := func(name string, pick func(result) time.Duration) {
		d := make([]time.Duration, len(okr))
		for i, r := range okr {
			d[i] = pick(r)
		}
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		fmt.Printf("%-14s %8s %8s %8s %8s\n", name,
			ms(pct(d, 50)), ms(pct(d, 95)), ms(pct(d, 99)), ms(d[len(d)-1]))
	}
	row("connect+auth", func(r result) time.Duration { return r.dial })
	row("session+pty", func(r result) time.Duration { return r.session })
	row("command", func(r result) time.Duration { return r.firstByte })
}

// classify groups failures so a hundred identical refusals read as one line.
func classify(err error) string {
	s := err.Error()
	for _, marker := range []string{
		"rate limit", "too many", "connection refused", "connection reset",
		"i/o timeout", "EOF", "handshake failed", "no route", "broken pipe",
	} {
		if strings.Contains(strings.ToLower(s), marker) {
			return marker
		}
	}
	return trim(s)
}

func pct(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := (len(sorted)*p + 99) / 100
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func ms(d time.Duration) string { return fmt.Sprintf("%.0fms", float64(d)/1e6) }

func trim(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > 70 {
		return s[:70] + "…"
	}
	return s
}

func loadKey(path string) (ssh.Signer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(b)
}

func die(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "loadtest: "+f+"\n", a...)
	os.Exit(1)
}
