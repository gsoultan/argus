package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

/*
The control server's own helper.

Logs, operating system updates, disks and terminals all reach a server through
its agent, and argus-control runs with no privileges at all
(packaging/systemd/argus-control.service). So the one server that holds the
evidence was the one the console could not look after: its logs filled, its
packages aged and its disk was grown over plain SSH, with nothing in the audit
chain to say who did it.

The argus-control package now ships the agent as argus-control-host, with a
unit of its own (packaging/systemd/argus-control-host.service). Nobody installs
or enrolls it:

  - argus-control mints a single-use enrollment token for its own hostname,
    marked control_host, and writes it with the address agents reach it at to
    <data dir>/host/enroll.json, readable by its own account and root only.
  - argus-control-host-enroll.path sees the file and starts the helper, whose
    ExecStartPre (`argus-control-host enroll-local`) exchanges the token the
    ordinary way, through POST /api/v1/agents/enroll.
  - That enrollment adds the server to the inventory, marked control_host, or
    marks the one already there. The file is removed once a live credential
    exists, and a new token is minted only while none does.

From then on it is an enrolled server like any other: the same Updates, Logs,
Disks and Terminal tabs, the same plans and re-checks, the same recordings and
the same audit entries, because it is the same code. What differs is said
where it differs:

  - It is never told to hook sshd. Plain SSH to the control server is not
    gated by Argus, deliberately: it is how anyone gets back in when Argus
    itself is what is broken. The console says so on the server's page.
  - It does not update or restart from the console (controlHostUpdateStatus):
    it moves with argus-control, under Settings > Control plane > Updates.
  - It is not removed from the inventory: the package would only enroll it
    again. Turning it off is `control_host: false` in the configuration.

A host that already runs argus-agent is left alone: the package disables the
unit where argus-agent is enabled (an installed but disabled argus-agent does
not count), the unit conflicts with argus-agent.service, and the token is
never minted while the hostname has a live credential, whoever's it is.
*/

// ControlHostRequestFile is the file argus-control writes for the helper, in
// ControlHostOptions.Dir.
const ControlHostRequestFile = "enroll.json"

// controlHostCreator is who a control-host token is made by, in the token's
// row, the audit chain and the enrollment's entry.
const controlHostCreator = "argus-control"

// controlHostTokenTTL is how long a minted token lasts. The longest a token
// may: the helper normally takes it within seconds of the file appearing,
// and one a day is all that reaches the audit chain while it does not.
const controlHostTokenTTL = MaxEnrollTokenTTL

// controlHostRenewBefore is how close to expiry a token is replaced.
const controlHostRenewBefore = time.Hour

// controlHostUpdateReason is what the console says instead of an Update or
// a Restart button for the helper.
const controlHostUpdateReason = "this is the helper on argus-control's own server: it updates and " +
	"restarts with argus-control, under Settings, Control plane, Updates"

// ErrControlHostAgent refuses an agent update or restart of the helper.
var ErrControlHostAgent = errors.New(controlHostUpdateReason)

// ErrControlHostAsset refuses removing the control server from the inventory.
var ErrControlHostAsset = errors.New("this is the server argus-control runs on, looked after " +
	"through the helper the argus-control package ships; it cannot be removed from Argus. " +
	"To stop managing it, set control_host: false in argus-control's configuration and " +
	"revoke its agent under Settings")

// ControlHostRequest is enroll.json: what the helper needs to enroll.
type ControlHostRequest struct {
	ControlURL string    `json:"controlUrl"`
	Token      string    `json:"token"`
	Pin        string    `json:"pin,omitempty"`
	Hostname   string    `json:"hostname"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

// ControlHostOptions says where the request goes and what it holds.
type ControlHostOptions struct {
	// Dir holds enroll.json. Made 0700 if missing.
	Dir string
	// Hostname is this host's name, as the helper will enroll under it.
	Hostname string
	// ControlURL is where the helper reaches this process: ControlHostURL
	// of the listener agents use.
	ControlURL string
	// Pin is the key the helper pins; nil when the certificate is not this
	// process's own.
	Pin func() (string, error)
	// Every is how often the loop looks; 0 is five minutes.
	Every time.Duration
}

// ControlHostURL is the address the helper on this host reaches the
// listener at addr through: loopback when the listener takes every address,
// else the address it is bound to. Plain HTTP is refused: the helper sends
// its credential with every report.
func ControlHostURL(addr string, tls bool) (string, error) {
	if !tls {
		return "", errors.New("argus-control does not serve TLS itself, so the helper on its own " +
			"server could not reach it safely; serve TLS from argus-control (tls: in the " +
			"configuration) for the console to look after this server")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("listen address %q: %w", addr, err)
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	return "https://" + net.JoinHostPort(host, port), nil
}

// liveCredentialFor says whether hostname has a live agent credential.
func (s *Store) liveCredentialFor(ctx context.Context, hostname string) (bool, error) {
	var live bool
	err := s.db(ctx).QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM agent_credentials
		                WHERE lower(hostname) = lower($1) AND revoked_at IS NULL)`,
		hostname).Scan(&live)
	return live, err
}

// MarkControlHostAsset puts the control server in the inventory, or marks
// the asset already there for hostname (by name or by its agent's name, an
// archived one brought back), and reports whether it had to add one.
func (s *Store) MarkControlHostAsset(ctx context.Context, hostname string) (Asset, bool, error) {
	var id string
	err := s.db(ctx).QueryRow(ctx, `
		UPDATE assets SET control_host = true, archived_at = NULL
		 WHERE id = (SELECT id FROM assets
		              WHERE lower(hostname) = lower($1) OR lower(agent_hostname) = lower($1)
		              ORDER BY archived_at IS NOT NULL, lower(hostname) = lower($1) DESC
		              LIMIT 1)
		RETURNING id::text`, hostname).Scan(&id)
	if err == nil {
		a, err := s.AssetByID(ctx, id)
		return a, false, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Asset{}, false, err
	}
	// No principals: who may open a terminal here, and as which account, is
	// chosen on the server's Access tab like any other server's.
	a, err := s.CreateAsset(ctx, AssetInput{
		Hostname: hostname, Address: "127.0.0.1", Protocol: "ssh", Kind: "server",
		GroupName: "argus",
	})
	if err != nil {
		return Asset{}, false, err
	}
	if _, err := s.db(ctx).Exec(ctx,
		`UPDATE assets SET control_host = true WHERE id = $1::uuid`, a.ID); err != nil {
		return Asset{}, false, err
	}
	a.ControlHost = true
	return a, true, nil
}

// RunControlHost keeps a request for the helper on this host while it has no
// live credential, until ctx ends.
func (a *API) RunControlHost(ctx context.Context, opts ControlHostOptions) {
	if opts.Every <= 0 {
		opts.Every = 5 * time.Minute
	}
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		a.log.Error("control host: cannot make the request directory", "dir", opts.Dir, "error", err)
		return
	}
	// The server argus-control runs on is the install's, so its asset,
	// token and audit entries are the default organization's. Without this
	// a strict store refuses every query the loop makes.
	ctx = PlatformContext(ctx)
	t := time.NewTicker(opts.Every)
	defer t.Stop()
	for {
		if err := a.controlHostTick(ctx, opts, time.Now()); err != nil && ctx.Err() == nil {
			a.log.Error("control host: cannot ask the helper on this server to enroll", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// controlHostTick is one look: remove the request once the host is enrolled,
// keep one that is still good, else mint a token and write a new one.
func (a *API) controlHostTick(ctx context.Context, opts ControlHostOptions, now time.Time) error {
	path := filepath.Join(opts.Dir, ControlHostRequestFile)
	live, err := a.store.liveCredentialFor(ctx, opts.Hostname)
	if err != nil {
		return err
	}
	if live {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if old, err := readControlHostRequest(path); err == nil &&
		strings.EqualFold(old.Hostname, opts.Hostname) && old.ControlURL == opts.ControlURL &&
		old.ExpiresAt.Sub(now) > controlHostRenewBefore {
		return nil
	}

	tok, err := a.store.createEnrollToken(ctx, controlHostCreator, opts.Hostname, controlHostTokenTTL, true)
	if err != nil {
		return err
	}
	req := ControlHostRequest{
		ControlURL: opts.ControlURL, Token: tok.Token, Hostname: opts.Hostname, ExpiresAt: tok.ExpiresAt,
	}
	if opts.Pin != nil {
		if req.Pin, err = opts.Pin(); err != nil {
			return fmt.Errorf("pin of this process's certificate: %w", err)
		}
	}
	if err := writeControlHostRequest(path, req); err != nil {
		return err
	}
	if _, err := a.store.AppendAudit(ctx, AuditEvent{
		Action:     "agent.enroll_token.created",
		Severity:   "info",
		ActorEmail: controlHostCreator,
		Target:     opts.Hostname,
		Detail: "argus-control made a single-use enrollment token for the helper on its own server, " +
			opts.Hostname + ", valid until " + tok.ExpiresAt.UTC().Format(time.RFC3339) +
			" (token " + tok.ID + "). It is left where only argus-control and root can read it, " +
			"and enrolling with it adds this server to Argus so the console can look after it.",
	}); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
	a.log.Info("control host: asked the helper on this server to enroll",
		"hostname", opts.Hostname, "control_url", opts.ControlURL)
	return nil
}

// readControlHostRequest reads a request this process wrote earlier.
func readControlHostRequest(path string) (ControlHostRequest, error) {
	var r ControlHostRequest
	f, err := os.Open(path)
	if err != nil {
		return r, err
	}
	defer f.Close()
	err = json.NewDecoder(io.LimitReader(f, 16<<10)).Decode(&r)
	return r, err
}

// writeControlHostRequest writes the request whole or not at all, 0600: it
// holds a token that adds a server to Argus.
func writeControlHostRequest(path string, r ControlHostRequest) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".enroll-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// controlHostEnrolled is the helper on argus-control's own server enrolling:
// the server goes into the inventory, marked, and the audit chain says who
// authorised it (argus-control's configuration did) and what it means.
func (a *API) controlHostEnrolled(r *http.Request, hostname string, enr Enrollment) {
	ctx := r.Context()
	asset, added, err := a.store.MarkControlHostAsset(ctx, hostname)
	inventory := "It is the server " + asset.Hostname + " in the inventory, now marked as argus-control's own."
	switch {
	case err != nil:
		a.log.Error("control host: cannot put this server in the inventory",
			"hostname", hostname, "error", err)
		inventory = "Putting it in the inventory failed (" + err.Error() + "); add " + hostname +
			" under Servers to look after it."
	case added:
		inventory = "It was added to the inventory as " + asset.Hostname + ", with nobody " +
			"assigned to it yet: choose who may open a terminal there on its Access tab."
	}
	if _, aerr := a.store.AppendAudit(ctx, AuditEvent{
		Action:     "agent.enrolled",
		Severity:   "notice",
		ActorEmail: controlHostCreator,
		Target:     hostname,
		Detail: "The helper the argus-control package ships enrolled on argus-control's own " +
			"server, " + hostname + ", from " + a.clientAddr(r) + " with a token argus-control " +
			"made for it (token " + enr.TokenID + ", credential " + enr.CredentialID + "), so " +
			"the console can clean its logs, update its packages, grow its disks and open a " +
			"terminal on it, each audited as on any other server. It may report only as " +
			hostname + ". Plain SSH to it is not gated by Argus, so it can still be reached " +
			"when Argus cannot. " + inventory,
	}); aerr != nil {
		a.log.Error("audit append failed", "error", aerr)
	}
	a.alertServer(ctx, "", hostname+" (argus-control's own server) joined Argus",
		"The console can now look after the server argus-control runs on.")
}

// ControlHostSupport is "" where argus-control can look after its own
// server, else why not: the helper is a systemd unit the Linux package
// installs.
func ControlHostSupport() string {
	switch {
	case runtime.GOOS != "linux":
		return "looking after argus-control's own server needs Linux with systemd"
	case os.Getenv("INVOCATION_ID") == "":
		return "argus-control is not run by systemd, which starts the helper on its own server"
	}
	for _, d := range []string{"/lib/systemd/system", "/usr/lib/systemd/system", "/etc/systemd/system"} {
		if _, err := os.Stat(d + "/argus-control-host-enroll.path"); err == nil {
			return ""
		}
	}
	return "the helper for argus-control's own server is not installed: argus-control was not " +
		"installed from its package, or from one older than this version"
}
