package vault

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/gsoultan/argus/internal/secrets"
	"github.com/gsoultan/argus/internal/tlsconfig"
)

/*
The outbound tunnel: argus-secret dials argus-control, so a server that keeps
secrets needs no inbound port at all.

Enrolling exchanges a one-time token from the console for this server's own
credential, and learns argus-control's management key and the organization
over the same TLS connection the token was sent on (pinned, or verified
against a CA). argus-secret then holds one connection to
argus-control (GET /api/v1/secrets/tunnel, upgraded to yamux) and answers on
streams argus-control opens: management requests, still each signed, and
agents' and apps' reads that argus-control relays. Values stay sealed to the
reader's key on the way, so the relay changes nothing about who can open
them.
*/

// Tunnel constants shared with argus-control.
const (
	TunnelPath         = "/api/v1/secrets/tunnel"
	EnrollPath         = "/api/v1/secrets/enroll"
	TunnelUpgrade      = "argus-secret-tunnel/1"
	RemoteHeader       = "X-Argus-Remote"
	EnrollTokenPrefix  = "argus-secret-enrol-"
	ServerCredPrefix   = "argus-secretd-"
	enrollmentFileName = "enrollment.json"
)

// Enrollment is what enrolling left in the data directory.
type Enrollment struct {
	ControlURL string `json:"controlUrl"`
	// One way of trusting argus-control: a pin, a CA file, or neither for
	// the system's roots.
	Pin        string    `json:"pin,omitempty"`
	CAFile     string    `json:"caFile,omitempty"`
	Credential string    `json:"credential"`
	ServerID   string    `json:"serverId"`
	Org        string    `json:"organization"`
	ControlKey string    `json:"controlPublicKey"`
	EnrolledAt time.Time `json:"enrolledAt"`
}

// EnrollRequest is what argus-secret sends with its token.
type EnrollRequest struct {
	Token   string `json:"token"`
	Version string `json:"version"`
}

// EnrollResponse is what argus-control answers.
type EnrollResponse struct {
	ServerID   string `json:"serverId"`
	Org        string `json:"organization"`
	ControlKey string `json:"controlPublicKey"`
	Credential string `json:"credential"`
}

// LoadEnrollment reads the enrollment, or nil when there is none.
func LoadEnrollment(dataDir string) (*Enrollment, error) {
	path := filepath.Join(dataDir, enrollmentFileName)
	raw, err := secrets.ReadPrivate(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var e Enrollment
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &e, nil
}

func (e *Enrollment) tls() (*tls.Config, error) {
	if e.CAFile != "" {
		return tlsconfig.Client(tlsconfig.ClientOptions{CAFile: e.CAFile})
	}
	return tlsconfig.PinnedOrPublic(e.Pin)
}

// EnrollOptions is what the person running argus-secret enroll gave.
type EnrollOptions struct {
	DataDir    string
	ControlURL string
	Token      string
	Pin        string
	CAFile     string
	Version    string
	// Organization is the configured one, if any: enrolling into another
	// organization is refused.
	Organization string
}

// Enroll exchanges a token for this server's credential and keeps it.
func Enroll(ctx context.Context, o EnrollOptions) (*Enrollment, error) {
	u, err := url.Parse(strings.TrimRight(o.ControlURL, "/"))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("--control is argus-control's https:// address")
	}
	if !strings.HasPrefix(o.Token, EnrollTokenPrefix) {
		return nil, errors.New("--token is the argus-secret-enrol-… token from Settings › Secrets servers")
	}
	// The token names its organization ("…-enrol-<org>.<secret>", none for
	// the default), so a mismatch is refused before the token is spent.
	if org := tokenOrg(o.Token); o.Organization != "" && org != o.Organization {
		return nil, fmt.Errorf("the token is for organization %q, but this server is configured for %q", org, o.Organization)
	}
	e := &Enrollment{ControlURL: u.String(), Pin: o.Pin, CAFile: o.CAFile}
	tlsCfg, err := e.tls()
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(EnrollRequest{Token: o.Token, Version: o.Version})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.ControlURL+EnrollPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	cl := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg, Proxy: http.ProxyFromEnvironment},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("argus-control did not answer: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		var m struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &m)
		if m.Error == "" {
			m.Error = resp.Status
		}
		return nil, fmt.Errorf("argus-control refused: %s", m.Error)
	}
	var out EnrollResponse
	if err := json.Unmarshal(raw, &out); err != nil || !strings.HasPrefix(out.Credential, ServerCredPrefix) {
		return nil, errors.New("argus-control answered something argus-secret cannot read")
	}
	if _, err := ParseControlKey(out.ControlKey); err != nil {
		return nil, err
	}
	if out.Org == "" || (o.Organization != "" && out.Org != o.Organization) {
		return nil, fmt.Errorf("the token is for organization %q, but this server is configured for %q", out.Org, o.Organization)
	}
	e.Credential, e.ServerID, e.Org, e.ControlKey, e.EnrolledAt = out.Credential, out.ServerID, out.Org, out.ControlKey, time.Now().UTC()
	raw, _ = json.MarshalIndent(e, "", "  ")
	if err := os.MkdirAll(o.DataDir, 0o700); err != nil {
		return nil, err
	}
	if err := writeFile(filepath.Join(o.DataDir, enrollmentFileName), raw, 0o600); err != nil {
		return nil, err
	}
	return e, nil
}

func tokenOrg(token string) string {
	org, _, found := strings.Cut(strings.TrimPrefix(token, EnrollTokenPrefix), ".")
	if !found {
		return "default"
	}
	return org
}

// tunnelHandler is the API as it answers on the tunnel. Only argus-control
// is on the other end, so its word for who is asking (RemoteHeader) is taken.
func tunnelHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remote := "0.0.0.0"
		if a, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get(RemoteHeader))); err == nil {
			remote = a.Unmap().String()
		}
		r.Header.Del(RemoteHeader)
		r.Header.Del("X-Forwarded-For")
		r.Header.Del("CF-Connecting-IP")
		r.RemoteAddr = net.JoinHostPort(remote, "0")
		h.ServeHTTP(w, r)
	})
}

// RunTunnel holds the connection to argus-control until ctx ends,
// reconnecting with backoff.
func RunTunnel(ctx context.Context, e *Enrollment, h http.Handler, log *slog.Logger) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := serveTunnel(ctx, e, tunnelHandler(h), log)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		log.Warn("tunnel to argus-control ended; reconnecting", "error", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

// DialTunnel opens one tunnel. Exported for tests.
func DialTunnel(ctx context.Context, e *Enrollment) (*yamux.Session, error) {
	tlsCfg, err := e.tls()
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(e.ControlURL)
	if err != nil {
		return nil, err
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "443")
	}
	tlsCfg = tlsCfg.Clone()
	if tlsCfg.ServerName == "" {
		tlsCfg.ServerName = u.Hostname()
	}
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}, Config: tlsCfg}
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequest(http.MethodGet, e.ControlURL+TunnelPath, nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", TunnelUpgrade)
	req.Header.Set("Authorization", "Bearer "+e.Credential)
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = conn.Close()
		return nil, fmt.Errorf("argus-control refused the tunnel: %s %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	_ = conn.SetDeadline(time.Time{})
	if br.Buffered() > 0 {
		_ = conn.Close()
		return nil, errors.New("argus-control sent data before the tunnel opened")
	}
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	return yamux.Client(conn, cfg)
}

func serveTunnel(ctx context.Context, e *Enrollment, h http.Handler, log *slog.Logger) error {
	sess, err := DialTunnel(ctx, e)
	if err != nil {
		return err
	}
	log.Info("tunnel to argus-control open", "control", e.ControlURL)
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 30 * time.Second, MaxHeaderBytes: 32 << 10}
	stop := context.AfterFunc(ctx, func() { _ = sess.Close() })
	defer stop()
	err = srv.Serve(sess)
	_ = sess.Close()
	return err
}
