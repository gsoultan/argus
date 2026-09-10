// Command shadowprobe attaches to a live session as a read-only viewer and
// prints what it sees.
//
// Development only. It exists because shadowing is a claim that can only be
// checked by watching one session from another: an auditor is meant to be able
// to see what an operator is doing, live, without being able to touch it. That
// is two properties, and neither is visible from the code alone.
//
//	shadowprobe wss://127.0.0.1:8081/ws/shadow?session=<id>&ticket=<t>
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
)

func main() {
	seconds := flag.Int("seconds", 15, "how long to watch")
	write := flag.String("write", "", "attempt to send this to the session; a shadow must not be able to")
	flag.Parse()
	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: shadowprobe [-seconds n] [-write s] <wss url>")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(*seconds+10)*time.Second)
	defer cancel()

	// Development only: the gateway presents a self-signed certificate here.
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	conn, _, err := websocket.Dial(ctx, flag.Arg(0), &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		fmt.Println("dial:", err)
		os.Exit(1)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(32 << 20)
	fmt.Println("attached")

	if *write != "" {
		// A viewer that can type is not a viewer. Whether the gateway refuses
		// the write or ignores it, what matters is that nothing reaches the
		// target -- checked by watching the session's own output.
		err := conn.Write(ctx, websocket.MessageText, []byte(*write))
		fmt.Printf("write attempt: err=%v\n", err)
	}

	deadline := time.Now().Add(time.Duration(*seconds) * time.Second)
	var frames int
	var seen strings.Builder
	for time.Now().Before(deadline) {
		rctx, rcancel := context.WithDeadline(ctx, deadline)
		_, data, err := conn.Read(rctx)
		rcancel()
		if err != nil {
			fmt.Println("read ended:", err)
			break
		}
		frames++
		var m map[string]any
		if json.Unmarshal(data, &m) == nil {
			if d, ok := m["data"].(string); ok {
				seen.WriteString(d)
			}
			if t, ok := m["type"].(string); ok && frames <= 3 {
				fmt.Printf("frame %d: type=%s\n", frames, t)
			}
			continue
		}
		seen.Write(data)
	}

	fmt.Printf("frames %d\n", frames)
	out := seen.String()
	if len(out) > 400 {
		out = out[:400] + "…"
	}
	fmt.Printf("saw: %q\n", out)
}
