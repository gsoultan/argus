// Command filedrill moves a file through the console's Files the way the
// browser does: it uploads one, finds it in a listing of the account's home,
// downloads it again and checks the bytes and their hash came back whole.
//
// For the install drill: it proves files go through an installed control
// plane and agent as the account, on the host's own file system.
//
//	filedrill -url https://localhost:8080 -ca ca.crt -ticket T -target H -principal P -name x.txt
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/coder/websocket"
)

type entry struct {
	Name string `json:"name"`
}

type message struct {
	Type      string  `json:"type"`
	Path      string  `json:"path,omitempty"`
	Overwrite bool    `json:"overwrite,omitempty"`
	Data      string  `json:"data,omitempty"`
	Entries   []entry `json:"entries,omitempty"`
	Size      int64   `json:"size,omitempty"`
	SHA256    string  `json:"sha256,omitempty"`
}

func fail(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "filedrill: "+f+"\n", a...)
	os.Exit(1)
}

func main() {
	base := flag.String("url", "https://localhost:8080", "control plane")
	caFile := flag.String("ca", "", "CA that signed the control plane's certificate")
	ticket := flag.String("ticket", "", "ticket from /api/v1/terminal/ticket")
	target := flag.String("target", "", "host")
	principal := flag.String("principal", "", "account")
	name := flag.String("name", "through-argus-files.txt", "file to put in the account's home")
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
	u.Path = "/api/v1/files"
	u.RawQuery = url.Values{"target": {*target}, "principal": {*principal}, "ticket": {*ticket}}.Encode()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
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
	c.SetReadLimit(8 << 20)

	send := func(m message) {
		b, _ := json.Marshal(m)
		if err := c.Write(ctx, websocket.MessageText, b); err != nil {
			fail("send %s: %v", m.Type, err)
		}
	}
	// next is the next text message, with any binary before it.
	next := func() (message, []byte) {
		var bin []byte
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				fail("read: %v", err)
			}
			if typ == websocket.MessageBinary {
				bin = append(bin, data...)
				continue
			}
			var m message
			if json.Unmarshal(data, &m) != nil {
				fail("not a message: %q", data)
			}
			if m.Type == "error" {
				fail("the agent said: %s", m.Data)
			}
			return m, bin
		}
	}
	expect := func(want string) (message, []byte) {
		m, bin := next()
		for m.Type == "ready" || m.Type == "notice" {
			m, bin = next()
		}
		if m.Type != want {
			fail("got %q, want %q (%+v)", m.Type, want, m)
		}
		return m, bin
	}

	content := []byte(fmt.Sprintf("through Argus files at %s\r\nsecond line ✓\r\n", time.Now().Format(time.RFC3339Nano)))
	sum := sha256.Sum256(content)
	want := hex.EncodeToString(sum[:])

	send(message{Type: "upload", Path: *name, Overwrite: true})
	ready, _ := expect("upload-ready")
	if err := c.Write(ctx, websocket.MessageBinary, content); err != nil {
		fail("upload: %v", err)
	}
	send(message{Type: "upload-end"})
	done, _ := expect("upload-done")
	if done.Size != int64(len(content)) || done.SHA256 != want {
		fail("uploaded %d bytes %s, want %d %s", done.Size, done.SHA256, len(content), want)
	}

	send(message{Type: "list", Path: "~"})
	listing, _ := expect("listing")
	found := false
	for _, e := range listing.Entries {
		found = found || e.Name == *name
	}
	if !found {
		fail("%s is not in the listing of %s", *name, listing.Path)
	}

	send(message{Type: "download", Path: *name})
	expect("download-start")
	end, got := expect("download-end")
	if !bytes.Equal(got, content) || end.SHA256 != want {
		fail("downloaded %q (%s), want what was uploaded (%s)", got, end.SHA256, want)
	}
	fmt.Printf("path=%s\nhome=%s\nsha256=%s\n", ready.Path, listing.Path, want)
}
