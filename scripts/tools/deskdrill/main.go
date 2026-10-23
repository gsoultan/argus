// Command deskdrill opens a Remote Desktop the way the console does, waits
// for the desktop to draw, moves the pointer and types a key, and closes.
//
// For the Windows install drill: it proves a desktop opens through an
// installed control plane and agent, signed in as a real account with its
// password, and that the screen arrives.
//
//	deskdrill -url https://localhost:8080 -ca ca.crt -ticket T -target H -principal P \
//	    -password-env DRILL_PASSWORD
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/coder/websocket"

	"github.com/gsoultan/argus/internal/rdp"
)

func main() {
	base := flag.String("url", "https://localhost:8080", "control plane")
	caFile := flag.String("ca", "", "CA that signed the control plane's certificate")
	ticket := flag.String("ticket", "", "ticket from /api/v1/terminal/ticket")
	target := flag.String("target", "", "host")
	principal := flag.String("principal", "", "account")
	passwordEnv := flag.String("password-env", "", "environment variable holding the account's password")
	want := flag.Int("frames", 1, "screen updates to wait for")
	flag.Parse()

	pool := x509.NewCertPool()
	if *caFile != "" {
		pem, err := os.ReadFile(*caFile)
		if err != nil {
			fail("%v", err)
		}
		pool.AppendCertsFromPEM(pem)
	}
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}

	u, err := url.Parse(*base)
	if err != nil {
		fail("%v", err)
	}
	origin := u.Scheme + "://" + u.Host
	u.Scheme = map[string]string{"https": "wss", "http": "ws"}[u.Scheme]
	u.Path = "/api/v1/desktop"
	u.RawQuery = url.Values{"target": {*target}, "principal": {*principal},
		"ticket": {*ticket}}.Encode()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		HTTPClient: client, HTTPHeader: http.Header{"Origin": {origin}}})
	if err != nil {
		if resp != nil {
			fail("dial: %v (%s)", err, resp.Status)
		}
		fail("dial: %v", err)
	}
	defer c.CloseNow()
	c.SetReadLimit(4 << 20)

	password := ""
	if *passwordEnv != "" {
		password = os.Getenv(*passwordEnv)
	}
	logon, _ := json.Marshal(map[string]any{"type": "logon", "password": password,
		"width": 1024, "height": 768})
	if err := c.Write(ctx, websocket.MessageText, logon); err != nil {
		fail("%v", err)
	}

	send := func(v any) {
		b, _ := json.Marshal(v)
		if err := c.Write(ctx, websocket.MessageText, b); err != nil {
			fail("%v", err)
		}
	}
	frames, bytes := 0, 0
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			fail("the desktop ended without saying how, after %d updates: %v", frames, err)
		}
		if typ == websocket.MessageText {
			var m struct{ Type, Data, Session string }
			_ = json.Unmarshal(data, &m)
			switch m.Type {
			case "ready":
				fmt.Fprintf(os.Stderr, "session=%s\n", m.Session)
			case "error":
				fail("refused: %s", m.Data)
			}
			continue
		}
		if len(data) < rdp.FrameHeaderSize {
			continue
		}
		switch data[0] {
		case rdp.FrameReady:
			fmt.Fprintf(os.Stderr, "ready %dx%d\n",
				int(data[6])|int(data[7])<<8, int(data[8])|int(data[9])<<8)
			send(map[string]any{"type": "mouse", "x": 200, "y": 200})
			send(map[string]any{"type": "key", "code": "ShiftLeft", "down": true})
			send(map[string]any{"type": "key", "code": "ShiftLeft", "down": false})
		case rdp.FrameEncoded, rdp.FrameBitmap:
			frames++
			bytes += len(data)
			if frames >= *want {
				fmt.Fprintf(os.Stderr, "frames=%d bytes=%d\n", frames, bytes)
				_ = c.Close(websocket.StatusNormalClosure, "done")
				return
			}
		case rdp.FrameClosed:
			fail("the desktop closed after %d updates", frames)
		}
	}
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "deskdrill: "+format+"\n", a...)
	os.Exit(100)
}
