package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/caduceus/wire"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
The user that owns a database made from Argus: its password, getting it in
through PgBouncer, and checking it really signs in. See tunnel/dbowner.go
and plans/create-db-owner/design.md.

The password arrives in its own frame after the grant. It stays in this
process: Postgres gets a SCRAM verifier, SQL Server its own hash, MySQL and
MongoDB the password as a parameter of the command that sets it. It is used
once more to sign in as the new user, then dropped.

PgBouncer: a Postgres entry that goes through PgBouncer must keep working
for the app. Argus only ever adds its own lines, each after a "# argus:"
line, to pgbouncer.ini's [databases] and to userlist.txt, keeps a copy of
each file first, and reloads PgBouncer with SIGHUP, which drops nothing.
When it cannot (the file is read-only, in a container it cannot write, or
the reload is refused) it says the exact lines to add by hand.
*/

const passwordWait = 15 * time.Second

// readCreatePassword reads the one FrameCreatePassword the control plane
// sends after the grant.
func readCreatePassword(ctx context.Context, r io.Reader) (string, error) {
	type got struct {
		pw  string
		err error
	}
	ch := make(chan got, 1)
	go func() {
		fr := tunnel.NewFrameReader(r)
		for {
			typ, p, err := fr.Next()
			if err != nil {
				ch <- got{err: err}
				return
			}
			if typ == tunnel.FrameCreatePassword {
				ch <- got{pw: string(p)}
				return
			}
		}
	}()
	t := time.NewTimer(passwordWait)
	defer t.Stop()
	select {
	case g := <-ch:
		if g.err != nil {
			return "", errors.New("the password did not arrive: " + g.err.Error())
		}
		return g.pw, nil
	case <-t.C:
		return "", errors.New("the password did not arrive in time")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// findBouncer is the PgBouncer listening on port, if this server has one.
func findBouncer(l pgLocator, port int) (pgBouncerSeen, bool) {
	if port <= 0 {
		return pgBouncerSeen{}, false
	}
	for _, b := range l.locate().Bouncers {
		if b.Port == port {
			return b, true
		}
	}
	return pgBouncerSeen{}, false
}

// poolerAuth says how a PgBouncer learns passwords.
func poolerAuth(b pgBouncerSeen) string {
	switch {
	case b.AuthType == "trust" || b.AuthType == "any":
		return "trust"
	case strings.TrimSpace(b.AuthQuery) == tunnel.PoolerAuthQuery:
		return "argus"
	case b.AuthQuery != "" && b.AuthUser != "":
		return "query"
	}
	return "file"
}

func poolerCovers(b pgBouncerSeen, name string) bool {
	for _, t := range b.Targets {
		if t.Name == "*" || t.Name == name {
			return true
		}
	}
	return false
}

// writable says the agent can change path without changing who owns it.
func writable(path string) bool {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

func poolerInfo(l pgLocator, b pgBouncerSeen) *tunnel.PoolerInfo {
	mode := b.PoolMode
	if mode == "" {
		mode = "session"
	}
	return &tunnel.PoolerInfo{Port: b.Port, Config: b.Config, Covers: poolerCovers(b, "\x00"), Auth: poolerAuth(b),
		Mode: mode, Writable: writable(l.path(b.Config))}
}

// poolerTargetFor is where a new [databases] line sends a database: where
// the other databases on this PgBouncer go, else the entry's own place.
func poolerTargetFor(b pgBouncerSeen, host string, port int) (string, int) {
	for _, t := range b.Targets {
		if t.Name != "*" && t.Host != "" {
			return t.Host, t.Port
		}
	}
	return host, port
}

// addArgusLine adds line to path, after a "# argus:" line saying why,
// under [section] when section is not "" (made if missing). It keeps a copy
// of the file as path.argus.bak first, and writes in place, so the file
// keeps its owner and mode. It does nothing when the line is there.
func addArgusLine(path, section, why, line string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil && !(section == "" && os.IsNotExist(err)) {
		return false, err
	}
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) == line {
			return false, nil
		}
	}
	if len(data) > 0 {
		if err := os.WriteFile(path+".argus.bak", data, 0o600); err != nil {
			return false, err
		}
	}
	add := tunnel.PoolerLineMarker + " " + why + "\n" + line + "\n"
	var out bytes.Buffer
	if section == "" {
		out.Write(data)
		if len(data) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
			out.WriteByte('\n')
		}
		out.WriteString(add)
	} else {
		done := false
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 64*1024), 4<<20)
		for sc.Scan() {
			out.WriteString(sc.Text() + "\n")
			t := strings.ToLower(strings.TrimSpace(sc.Text()))
			if !done && t == "["+section+"]" {
				out.WriteString(add)
				done = true
			}
		}
		if !done {
			out.WriteString("\n[" + section + "]\n" + add)
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0o640)
	if err != nil {
		return false, err
	}
	if _, err := f.Write(out.Bytes()); err != nil {
		_ = f.Close()
		return false, err
	}
	return true, f.Close()
}

// reloadBouncers tells every PgBouncer on this server to read its files
// again. SIGHUP is PgBouncer's RELOAD: no connection is dropped. A test
// server under a made-up root has no processes to signal.
var reloadBouncers = func(l pgLocator) error {
	n := 0
	for _, p := range l.processes() {
		if p.comm != "pgbouncer" {
			continue
		}
		pid, err := strconv.Atoi(p.pid)
		if err != nil {
			continue
		}
		if err := hangUp(pid); err != nil {
			return err
		}
		n++
	}
	if n == 0 {
		return errors.New("no running PgBouncer to reload")
	}
	return nil
}

// poolerPlan is what making the database reachable through PgBouncer
// changes, as text for the preview, the result and Activity.
type poolerPlan struct {
	route, user string // lines to add; "" when nothing is needed
}

func (p poolerPlan) text(b pgBouncerSeen) []string {
	var out []string
	if p.route != "" {
		out = append(out, b.Config+" [databases]: "+p.route)
	}
	if p.user != "" {
		out = append(out, b.AuthFile+": \""+strings.SplitN(p.user, `"`, 3)[1]+"\" "+tunnel.PasswordRedacted)
	}
	if len(out) > 0 {
		out = append(out, "RELOAD PgBouncer")
	}
	return out
}

// ensurePooler makes the new database, and its new owner, reachable
// through PgBouncer b. It returns the steps, what changed as text, and the
// lines a person must add when the agent could not.
func ensurePooler(l pgLocator, b pgBouncerSeen, c tunnel.GrantCreate, verifier string) (steps []tunnel.CreateStep, changed, manual []string) {
	var p poolerPlan
	if !poolerCovers(b, c.Name) {
		host, port := poolerTargetFor(b, c.Host, c.Port)
		p.route = tunnel.PoolerDatabaseLine(c.Name, host, port)
	}
	if c.NewOwner() && poolerAuth(b) == "file" && b.AuthFile != "" && verifier != "" {
		p.user = tunnel.PoolerUserLine(c.Owner.Name, verifier)
	}
	if p.route == "" && p.user == "" {
		steps = append(steps, tunnel.CreateStep{OK: true, Text: fmt.Sprintf("PgBouncer %d already sends %s to Postgres", b.Port, c.Name)})
		return steps, nil, nil
	}
	wrote := false
	var failed error
	if p.route != "" {
		ok, err := addArgusLine(l.path(b.Config), "databases", "added by Argus for "+c.Name, p.route)
		wrote = wrote || ok
		failed = errors.Join(failed, err)
	}
	if p.user != "" && failed == nil {
		ok, err := addArgusLine(l.path(b.AuthFile), "", "added by Argus, owner of "+c.Name, p.user)
		wrote = wrote || ok
		failed = errors.Join(failed, err)
	}
	if failed == nil && wrote {
		failed = reloadBouncers(l)
	}
	if failed != nil {
		if p.route != "" {
			manual = append(manual, "# "+b.Config+", under [databases]", p.route)
		}
		if p.user != "" {
			manual = append(manual, "# "+b.AuthFile+" (the SCRAM hash, not the password)", p.user)
		}
		manual = append(manual, "# then reload PgBouncer (RELOAD, or systemctl reload pgbouncer)")
		steps = append(steps, tunnel.CreateStep{Text: fmt.Sprintf("Send %s through PgBouncer %d", c.Name, b.Port),
			Error: "the agent could not change PgBouncer here (" + failed.Error() + "); add the lines shown by hand"})
		return steps, nil, manual
	}
	steps = append(steps, tunnel.CreateStep{OK: true, Text: fmt.Sprintf("PgBouncer %d sends %s to Postgres and was reloaded; nothing was dropped", b.Port, c.Name)})
	return steps, p.text(b), nil
}

// pgSignIn signs in as user with pw to database on 127.0.0.1:port, the
// way an app on this server would, to prove the password and the route.
// PgBouncer reads its files again a moment after a reload, so a pooled
// check tries a few times.
var pgSignIn = func(ctx context.Context, port int, user, pw, database string, tries int) error {
	cfg, err := wire.ParseConfig("host=127.0.0.1 sslmode=prefer connect_timeout=5")
	if err != nil {
		return err
	}
	cfg.Port, cfg.User, cfg.Password, cfg.Database = uint16(port), user, pw, database
	var last error
	for i := 0; i < tries; i++ {
		if i > 0 {
			select {
			case <-time.After(700 * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		conn, err := pgConnectConfig(ctx, cfg)
		if err == nil {
			_ = conn.Close(context.WithoutCancel(ctx))
			return nil
		}
		last = err
	}
	return last
}

func signInText(err error) string {
	var pe *wire.Error
	if errors.As(err, &pe) {
		return pe.Message
	}
	return err.Error()
}
