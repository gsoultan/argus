// Command termdrill opens a browser terminal the way the console does, types
// a script into it, and exits with the remote shell's exit status.
//
// For the install drill: it proves a session opens through an installed
// control plane and agent, as a real account, outside the agent's sandbox.
//
//	termdrill -url https://localhost:8080 -ca ca.crt -ticket T -target H -principal P \
//	    -input 'id -un; exit 7'
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
)

type message struct {
	Type      string `json:"type"`
	Data      string `json:"data,omitempty"`
	Session   string `json:"session,omitempty"`
	ChainHead string `json:"chain_head,omitempty"`
	ExitCode  int    `json:"exit_code,omitempty"`
}

func main() {
	base := flag.String("url", "https://localhost:8080", "control plane")
	caFile := flag.String("ca", "", "CA that signed the control plane's certificate")
	ticket := flag.String("ticket", "", "terminal ticket from /api/v1/terminal/ticket")
	target := flag.String("target", "", "host")
	principal := flag.String("principal", "", "account")
	input := flag.String("input", "exit\n", "typed once the session is ready")
	// cmd.exe on a pseudoconsole drops the Enter of anything typed before it
	// has drawn its prompt: the text is echoed and never run. A person sees
	// the prompt before typing; this waits for it, and types a line at a time.
	afterOutput := flag.Bool("after-output", false,
		"wait for the shell's first output, then type the input one line at a time")
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
	u.Path = "/api/v1/terminal"
	u.RawQuery = url.Values{"target": {*target}, "principal": {*principal},
		"ticket": {*ticket}}.Encode()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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

	send := func(text string) {
		b, _ := json.Marshal(message{Type: "input", Data: text})
		if err := c.Write(ctx, websocket.MessageText, b); err != nil {
			fail("%v", err)
		}
	}
	typed := false
	typeLines := func() {
		typed = true
		go func() {
			time.Sleep(time.Second)
			for _, line := range lines(*input) {
				send(line)
				time.Sleep(300 * time.Millisecond)
			}
		}()
	}

	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			fail("the session ended without saying how: %v", err)
		}
		if typ == websocket.MessageBinary {
			_, _ = os.Stdout.Write(data)
			if *afterOutput && !typed {
				typeLines()
			}
			continue
		}
		var m message
		_ = json.Unmarshal(data, &m)
		switch m.Type {
		case "ready":
			fmt.Fprintf(os.Stderr, "session=%s\n", m.Session)
			if !*afterOutput {
				send(*input)
			}
		case "error":
			fail("refused: %s", m.Data)
		case "closed":
			fmt.Fprintf(os.Stderr, "\nexit=%d chain_head=%s\n", m.ExitCode, m.ChainHead)
			os.Exit(m.ExitCode)
		}
	}
}

// lines splits input after each \r or \n, keeping the terminator.
func lines(input string) []string {
	var out []string
	start := 0
	for i := 0; i < len(input); i++ {
		if input[i] == '\r' || input[i] == '\n' {
			out = append(out, input[start:i+1])
			start = i + 1
		}
	}
	if start < len(input) {
		out = append(out, input[start:])
	}
	return out
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "termdrill: "+format+"\n", a...)
	os.Exit(100)
}
