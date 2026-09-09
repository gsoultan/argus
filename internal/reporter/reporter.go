// Package reporter sends session and liveness records to the control plane.
//
// Every send is best-effort and non-blocking. A control-plane outage must never
// stop a session being brokered or recorded — the artefact on disk is the thing
// that matters, and reporting is how it becomes visible, not how it becomes
// real. Reports that fail are spooled and retried.
package reporter

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Client posts records to argus-control.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
	log     *slog.Logger

	// spoolPath holds reports that could not be delivered. Without it a
	// control-plane restart would lose every session that happened during it.
	spoolPath string
	mu        sync.Mutex

	// MaxSpoolBytes bounds the spool. Zero means DefaultMaxSpoolBytes.
	//
	// It was unbounded. A control plane that is unreachable for a week -- which
	// is exactly what a misconfigured agent does, silently -- grows this file
	// without limit on the host that must keep recording. A full disk then
	// stops the recorder, and a session that cannot be recorded is terminated,
	// so an unreachable control plane eventually takes privileged access down
	// with it. Measured on the dev fleet: 11.9 MB and climbing.
	MaxSpoolBytes int64

	// failures counts consecutive delivery failures, and firstFailure is when
	// the run started. A single warning per report is indistinguishable from
	// noise; an operator needs to be told that evidence has stopped arriving.
	failures     int
	firstFailure time.Time
	lastEscalate time.Time
}

// New builds a reporter. An empty baseURL disables reporting entirely, which is
// how the gateway runs standalone.
func New(baseURL, token, spoolPath string, log *slog.Logger) *Client {
	return NewWithTLS(baseURL, token, spoolPath, nil, log)
}

// NewWithTLS builds a reporter that verifies the control plane's certificate,
// and optionally presents a client certificate of its own.
//
// This link carries session records and audit events. Without verification an
// attacker who can intercept it can forge or suppress the evidence of their own
// session, which is the one thing the product exists to prevent.
func NewWithTLS(baseURL, token, spoolPath string, tlsCfg *tls.Config, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if tlsCfg != nil {
		transport.TLSClientConfig = tlsCfg
	}
	return &Client{
		baseURL:   baseURL,
		token:     token,
		spoolPath: spoolPath,
		log:       log,
		http: &http.Client{
			// Short: a slow control plane must not hold a session's cleanup.
			Timeout:   5 * time.Second,
			Transport: transport,
		},
	}
}

// Enabled reports whether a control plane is configured.
func (c *Client) Enabled() bool { return c != nil && c.baseURL != "" }

// Session reports a session record.
func (c *Client) Session(ctx context.Context, v any) {
	c.post(ctx, "/api/v1/report/session", v)
}

// Heartbeat reports agent liveness and posture.
func (c *Client) Heartbeat(ctx context.Context, v any) {
	c.post(ctx, "/api/v1/report/heartbeat", v)
}

// Facts reports what a host is: its OS, addresses, SSH ports and login
// accounts. Sent on a slower cadence than the heartbeat because these change on
// a reboot, not every thirty seconds.
func (c *Client) Facts(ctx context.Context, v any) {
	c.post(ctx, "/api/v1/report/facts", v)
}

// Asset registers or updates an asset.
func (c *Client) Asset(ctx context.Context, v any) {
	c.post(ctx, "/api/v1/report/asset", v)
}

// Audit appends an audit event.
func (c *Client) Audit(ctx context.Context, v any) {
	c.post(ctx, "/api/v1/report/audit", v)
}

type spoolEntry struct {
	Path string          `json:"path"`
	Body json.RawMessage `json:"body"`
	At   time.Time       `json:"at"`
}

func (c *Client) post(ctx context.Context, path string, v any) {
	if !c.Enabled() {
		return
	}
	body, err := json.Marshal(v)
	if err != nil {
		c.log.Error("marshal report", "path", path, "error", err)
		return
	}
	if err := c.send(ctx, path, body); err != nil {
		c.log.Warn("control plane unreachable, spooling report",
			"path", path, "error", err)
		c.spool(spoolEntry{Path: path, Body: body, At: time.Now().UTC()})
		c.noteFailure(err)
		return
	}
	c.noteSuccess()
}

// DefaultMaxSpoolBytes bounds the undelivered-report file.
//
// Generous enough to survive a long control-plane outage on a busy gateway, and
// small enough that it cannot be what fills the disk.
const DefaultMaxSpoolBytes = 64 << 20 // 64 MiB

// escalateAfter is how long a run of failures must last before it stops being
// a warning and becomes an error, and how often to repeat it.
//
// Reports are frequent, so the first minute of an outage is ordinary. An hour
// of silence is a fleet that has stopped producing evidence.
const escalateAfter = 5 * time.Minute

// noteFailure records a delivery failure and escalates a sustained run of them.
//
// One warning per failed report reads as noise. What an operator needs to know
// is that reports have not been landing for twenty minutes -- because for that
// whole time the console has been showing a fleet that looks fine.
func (c *Client) noteFailure(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	if c.failures == 0 {
		c.firstFailure = now
		c.lastEscalate = now
	}
	c.failures++

	since := now.Sub(c.firstFailure)
	if since < escalateAfter || now.Sub(c.lastEscalate) < escalateAfter {
		return
	}
	c.lastEscalate = now
	c.log.Error("nothing has reached the control plane for a sustained period",
		"for", since.Round(time.Second), "failed_reports", c.failures, "error", err,
		"detail", "sessions and audit events are being spooled, not delivered; "+
			"the console is showing a fleet it has stopped hearing from")
}

// noteSuccess ends a run of failures, and says so if there was one worth
// mentioning -- an operator who saw the alarm needs to see it clear.
func (c *Client) noteSuccess() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failures == 0 {
		return
	}
	if since := time.Since(c.firstFailure); since >= escalateAfter {
		c.log.Info("the control plane is reachable again",
			"was_unreachable_for", since.Round(time.Second),
			"failed_reports", c.failures)
	}
	c.failures = 0
}

func (c *Client) send(ctx context.Context, path string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("control plane returned %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) spool(e spoolEntry) {
	if c.spoolPath == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(c.spoolPath), 0o700); err != nil {
		return
	}
	c.trimSpoolLocked()

	f, err := os.OpenFile(c.spoolPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_ = json.NewEncoder(f).Encode(e)
}

// trimSpoolLocked drops the oldest reports once the spool exceeds its bound.
//
// Oldest first, deliberately. If some evidence has to go, the report an
// investigator is least likely to want is the one furthest in the past, and the
// alternative -- refusing to spool anything new -- silently discards whatever is
// happening right now, which is the part somebody is probably asking about.
//
// Losing evidence is not acceptable, but neither is filling the disk that the
// recorder is writing to: a full disk stops recording, and a session that
// cannot be recorded is terminated. Between losing the oldest spooled reports
// and taking privileged access down, this is the lesser harm, and it says so.
//
// Caller holds c.mu.
func (c *Client) trimSpoolLocked() {
	max := c.MaxSpoolBytes
	if max <= 0 {
		max = DefaultMaxSpoolBytes
	}
	fi, err := os.Stat(c.spoolPath)
	if err != nil || fi.Size() <= max {
		return
	}

	data, err := os.ReadFile(c.spoolPath)
	if err != nil {
		return
	}
	// Keep the newest half, so trimming is occasional rather than once per
	// report at the boundary.
	keepFrom := int64(len(data)) - max/2
	if keepFrom < 0 {
		keepFrom = 0
	}
	// Start at a record boundary; a half line would not survive a reload.
	if i := bytes.IndexByte(data[keepFrom:], '\n'); i >= 0 {
		keepFrom += int64(i) + 1
	}
	dropped := bytes.Count(data[:keepFrom], []byte{'\n'})

	tmp := c.spoolPath + ".trim"
	if err := os.WriteFile(tmp, data[keepFrom:], 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, c.spoolPath); err != nil {
		_ = os.Remove(tmp)
		return
	}
	c.log.Error("the report spool is full; the oldest undelivered reports were dropped",
		"dropped", dropped, "limit_bytes", max, "path", c.spoolPath,
		"detail", "the control plane has been unreachable long enough to fill "+
			"the spool; those sessions and audit events are lost, and the disk "+
			"this recorder writes to was the alternative")
}

// Drain retries spooled reports, returning how many were delivered.
//
// Rewrites the spool with only what still fails, so a permanently rejected
// report does not block everything behind it forever.
func (c *Client) Drain(ctx context.Context) int {
	if !c.Enabled() || c.spoolPath == "" {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	data, err := os.ReadFile(c.spoolPath)
	if err != nil {
		return 0
	}

	var remaining []byte
	delivered := 0
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var e spoolEntry
		if err := dec.Decode(&e); err != nil {
			break
		}
		if err := c.send(ctx, e.Path, e.Body); err != nil {
			line, _ := json.Marshal(e)
			remaining = append(remaining, line...)
			remaining = append(remaining, '\n')
			continue
		}
		delivered++
	}

	if len(remaining) == 0 {
		_ = os.Remove(c.spoolPath)
	} else {
		tmp := c.spoolPath + ".tmp"
		if err := os.WriteFile(tmp, remaining, 0o600); err == nil {
			_ = os.Rename(tmp, c.spoolPath)
		}
	}
	if delivered > 0 {
		c.log.Info("delivered spooled reports", "count", delivered)
	}
	return delivered
}

// StartDrainLoop retries the spool periodically until ctx is cancelled.
func (c *Client) StartDrainLoop(ctx context.Context, every time.Duration) {
	if !c.Enabled() {
		return
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				c.Drain(ctx)
			case <-ctx.Done():
				return
			}
		}
	}()
}
