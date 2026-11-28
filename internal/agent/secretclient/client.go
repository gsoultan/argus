// Package secretclient is the agent's side of argus-secret: it finds the
// server that keeps a name and reads from it directly, with this host's agent
// credential, the value sealed to a key made for that one read.
//
// argus-control is asked only where the servers are, and the answer is kept
// in the state directory, so a read still works while argus-control is down:
// argus-secret decides from the rules it already holds.
package secretclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/secretwire"
	"github.com/gsoultan/argus/internal/tlsconfig"
)

// Server is an argus-secret server, as argus-control describes it.
type Server struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	TLSPin string `json:"tlsPin,omitempty"`
	// Relay, for a server that dials argus-control rather than listening:
	// the path on argus-control that passes reads to it.
	Relay   string   `json:"relay,omitempty"`
	Folders []string `json:"folders"`
}

// Ref names a value: a secret, and optionally a field, a version or a label.
// Written prod/payments/db#password, prod/db@3 or prod/db@previous.
type Ref struct {
	Name    string
	Field   string
	Version int
	Label   string
}

// ParseRef reads name[#field][@version|@label].
func ParseRef(s string) (Ref, error) {
	var r Ref
	s = strings.TrimSpace(s)
	if at := strings.LastIndex(s, "@"); at >= 0 {
		v := s[at+1:]
		s = s[:at]
		if n, err := parsePositive(v); err == nil {
			r.Version = n
		} else if v != "" {
			r.Label = v
		}
	}
	if hash := strings.Index(s, "#"); hash >= 0 {
		r.Name, r.Field = s[:hash], s[hash+1:]
	} else {
		r.Name = s
	}
	if r.Name == "" || strings.ContainsAny(r.Name, " \t\n") || strings.HasPrefix(r.Name, "/") {
		return r, fmt.Errorf("%q is not a secret name: use folder/name[#field][@version]", s)
	}
	return r, nil
}

func parsePositive(s string) (int, error) {
	n := 0
	if s == "" || len(s) > 6 {
		return 0, errors.New("not a version")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("not a version")
		}
		n = n*10 + int(c-'0')
	}
	if n == 0 {
		return 0, errors.New("not a version")
	}
	return n, nil
}

// Client reads secrets for this host.
type Client struct {
	ControlURL string
	ControlTLS *tls.Config
	Credential string
	// StateDir is where the list of servers is kept between reads.
	StateDir string
	HTTP     func(pin string) (*http.Client, error)
	Now      func() time.Time
}

const (
	serversFile = "secrets-servers.json"
	timeout     = 15 * time.Second
	maxAnswer   = 1 << 20
	// refreshAfter is how long a kept list is used without asking again.
	refreshAfter = 5 * time.Minute
	// relayAttempts is how often a relayed read is tried while argus-control
	// answers that another replica holds the server's tunnel.
	relayAttempts = 3
)

// relayBackoff is the wait before the second relay attempt, and twice it
// before the third. A variable so tests need not wait.
var relayBackoff = 250 * time.Millisecond

func (c *Client) httpFor(pin string) (*http.Client, error) {
	if c.HTTP != nil {
		return c.HTTP(pin)
	}
	cfg, err := tlsconfig.PinnedOrPublic(pin)
	if err != nil {
		return nil, err
	}
	return &http.Client{Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     &http.Transport{TLSClientConfig: cfg, Proxy: http.ProxyFromEnvironment}}, nil
}

func (c *Client) controlHTTP() *http.Client {
	return &http.Client{Timeout: timeout, Transport: &http.Transport{TLSClientConfig: c.ControlTLS, Proxy: http.ProxyFromEnvironment},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

type keptServers struct {
	At      time.Time `json:"at"`
	Servers []Server  `json:"servers"`
}

// Servers are the organization's servers: from argus-control when it
// answers, else the list kept from the last time it did.
func (c *Client) Servers(ctx context.Context) ([]Server, error) {
	path := filepath.Join(c.StateDir, serversFile)
	var kept keptServers
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &kept)
		if c.now().Sub(kept.At) < refreshAfter && len(kept.Servers) > 0 {
			return kept.Servers, nil
		}
	}
	srvs, err := c.fetchServers(ctx)
	if err != nil {
		if len(kept.Servers) > 0 {
			return kept.Servers, nil
		}
		return nil, err
	}
	if raw, err := json.Marshal(keptServers{At: c.now().UTC(), Servers: srvs}); err == nil {
		tmp := path + ".tmp"
		if os.WriteFile(tmp, raw, 0o600) == nil {
			_ = os.Rename(tmp, path)
		}
	}
	return srvs, nil
}

func (c *Client) fetchServers(ctx context.Context) ([]Server, error) {
	if c.ControlURL == "" || c.Credential == "" {
		return nil, errors.New("this host is not enrolled: run argus-agent enroll first")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.ControlURL, "/")+"/api/v1/report/secrets-servers", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Credential)
	cl := c.controlHTTP()
	resp, err := cl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("argus-control did not answer: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("argus-control: %s", answerError(raw, resp.Status))
	}
	var srvs []Server
	if err := json.Unmarshal(raw, &srvs); err != nil {
		return nil, fmt.Errorf("argus-control answered something this agent cannot read: %w", err)
	}
	return srvs, nil
}

func answerError(raw []byte, status string) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error != "" {
		return e.Error
	}
	return status
}

// ErrNoServer is a name whose folder no server keeps.
var ErrNoServer = errors.New("no secrets server keeps this folder")

func serverFor(srvs []Server, name string) (Server, bool) {
	folder, _, _ := strings.Cut(name, "/")
	for _, s := range srvs {
		for _, f := range s.Folders {
			if f == folder {
				return s, true
			}
		}
	}
	return Server{}, false
}

// Value is a value read, with what it is.
type Value struct {
	Name    string
	Version int
	Field   string
	Bytes   []byte
}

// Read reads one value. purpose goes into the server's read log.
func (c *Client) Read(ctx context.Context, ref Ref, purpose string) (Value, error) {
	srvs, err := c.Servers(ctx)
	if err != nil {
		return Value{}, err
	}
	srv, ok := serverFor(srvs, ref.Name)
	if !ok {
		return Value{}, fmt.Errorf("%s: %w", ref.Name, ErrNoServer)
	}
	// A one-time post-quantum key (ML-KEM-768 + X25519): the value is sealed
	// to it, and only a format v2 answer is accepted.
	key, err := secretwire.NewPQKey()
	if err != nil {
		return Value{}, err
	}
	body, _ := json.Marshal(secretwire.ReadRequest{Name: ref.Name, Version: ref.Version, Label: ref.Label, Field: ref.Field,
		Recipient: secretwire.EncodeKey(key.PublicKey().Bytes()), Purpose: purpose})
	base := strings.TrimRight(srv.URL, "/")
	var cl *http.Client
	if srv.Relay != "" {
		// Through argus-control, trusted as the agent trusts it.
		if !strings.HasPrefix(srv.Relay, "/api/v1/secrets/relay/") || strings.Contains(srv.Relay, "..") {
			return Value{}, fmt.Errorf("%s: argus-control sent a relay path this agent does not follow", srv.Name)
		}
		base = strings.TrimRight(c.ControlURL, "/") + srv.Relay
		cl = c.controlHTTP()
	} else if cl, err = c.httpFor(srv.TLSPin); err != nil {
		return Value{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var resp *http.Response
	var raw []byte
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/read", bytes.NewReader(body))
		if err != nil {
			return Value{}, err
		}
		req.Header.Set("Authorization", "Agent "+c.Credential)
		req.Header.Set("Content-Type", "application/json")
		if resp, err = cl.Do(req); err != nil {
			return Value{}, fmt.Errorf("%s did not answer: %w", srv.Name, err)
		}
		raw, _ = io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
		resp.Body.Close()
		// A relay that reached the wrong replica of argus-control: another
		// connection may reach the one holding the server's tunnel.
		if srv.Relay == "" || resp.StatusCode != http.StatusServiceUnavailable || attempt == relayAttempts {
			break
		}
		cl.CloseIdleConnections()
		select {
		case <-ctx.Done():
			return Value{}, fmt.Errorf("%s did not answer: %w", srv.Name, ctx.Err())
		case <-time.After(time.Duration(attempt) * relayBackoff):
		}
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNotFound {
			return Value{}, fmt.Errorf("%s: no such secret, or this host may not read it", ref.Name)
		}
		return Value{}, fmt.Errorf("%s: %s", srv.Name, answerError(raw, resp.Status))
	}
	var out secretwire.ReadResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return Value{}, fmt.Errorf("%s answered something this agent cannot read", srv.Name)
	}
	// The answer must be for what was asked: a server cannot swap in another
	// secret, since the AAD it was sealed with names it.
	if out.Name != ref.Name || out.Field != ref.Field || (ref.Version != 0 && out.Version != ref.Version) {
		return Value{}, fmt.Errorf("%s answered for %s, not %s", srv.Name, out.Name, ref.Name)
	}
	plain, err := secretwire.OpenPQ(key, out.Sealed, secretwire.ReadAAD(out.Name, out.Version, out.Field))
	if err != nil {
		return Value{}, fmt.Errorf("%s: the value could not be opened", srv.Name)
	}
	return Value{Name: out.Name, Version: out.Version, Field: out.Field, Bytes: plain}, nil
}
