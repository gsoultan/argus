package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gsoultan/caduceus/wire"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
A Postgres on another server, reached from a server that runs only PgBouncer.

The agent normally signs in to its own server's Postgres as the postgres
account through peer authentication, and reaches nothing else. A server that
runs PgBouncer in front of a Postgres elsewhere has no such account to use,
but it has PgBouncer's configuration: pgbouncer.ini says which Postgres
servers it sends to, and its auth_file (userlist.txt) holds passwords.

So the agent signs in to a Postgres on another server only when both hold:

  - a pgbouncer.ini on this server sends a database to exactly that host and
    port. The server's own administrator chose it, not a grant; a grant
    naming any other address is refused, so the agent is still no way into
    the rest of the network.
  - that PgBouncer's userlist.txt has the admin user's password itself.
    A SCRAM or md5 hash there cannot sign in, and the answer says so.

The password is read when needed, used for one connection, and never leaves
this process.
*/

// pgRemoteWait bounds the connection to a Postgres on another server.
const pgRemoteWait = 15 * time.Second

// behindPooler finds the PgBouncer on this server that sends to host:port.
func (l pgLocator) behindPooler(host string, port int) (pgBouncerSeen, bool) {
	if tunnel.LocalPGHost(host) || !tunnel.RemotePGHost(host) {
		return pgBouncerSeen{}, false
	}
	for _, b := range l.locate().Bouncers {
		for _, t := range b.Targets {
			if strings.EqualFold(t.Host, host) && t.Port == port {
				return b, true
			}
		}
	}
	return pgBouncerSeen{}, false
}

// sentOn says where this server's PgBouncer sends a database whose entry
// names this server, when no Postgres runs here: an entry added before the
// agent could reach another server, or typed by hand, keeps the default
// socket, yet PgBouncer on this server sends the database elsewhere. Only
// a [databases] line for that name, or "*", counts, and only when every
// PgBouncer here agrees on one place, so the agent never guesses.
func (l pgLocator) sentOn(db tunnel.GrantDB) (tunnel.PoolerTarget, bool) {
	if !tunnel.LocalPGHost(db.Host) || db.Name == "" {
		return tunnel.PoolerTarget{}, false
	}
	loc := l.locate()
	if len(loc.Bouncers) == 0 {
		return tunnel.PoolerTarget{}, false
	}
	for _, p := range loc.Places {
		if l.live(p) {
			return tunnel.PoolerTarget{}, false
		}
	}
	if !strings.HasPrefix(db.Host, "/") {
		// A loopback address: a Postgres there listens on TCP only, which
		// nothing above sees, unless the port is PgBouncer's own.
		ours := false
		for _, b := range loc.Bouncers {
			ours = ours || b.Port == db.Port
		}
		if !ours {
			return tunnel.PoolerTarget{}, false
		}
	}
	for _, name := range []string{db.Name, "*"} {
		var found []tunnel.PoolerTarget
		named := false
		for _, b := range loc.Bouncers {
			for _, t := range b.Targets {
				if t.Name != name {
					continue
				}
				named = true
				if t.Local() || !tunnel.RemotePGHost(t.Host) {
					continue
				}
				if t.DBName == "" {
					t.DBName = db.Name
				}
				same := false
				for _, f := range found {
					same = same || (strings.EqualFold(f.Host, t.Host) && f.Port == t.Port && f.DBName == t.DBName)
				}
				if !same {
					found = append(found, t)
				}
			}
		}
		switch {
		case len(found) == 0 && named:
			// PgBouncer names the database and keeps it here; "*" is
			// not where it goes.
			return tunnel.PoolerTarget{}, false
		case len(found) == 0:
			continue
		case len(found) == 1:
			return found[0], true
		default:
			return tunnel.PoolerTarget{}, false
		}
	}
	return tunnel.PoolerTarget{}, false
}

func pgLocatorBehind(root, host string, port int) bool {
	_, ok := pgLocator{root: root}.behindPooler(host, port)
	return ok
}

// withPooledRemote sends a request for a Postgres on another server through
// pgRemoteAdmin, and every other to local.
func withPooledRemote(root string, local func(context.Context, PGAdminRequest) PGAdminResult) func(context.Context, PGAdminRequest) PGAdminResult {
	return func(ctx context.Context, req PGAdminRequest) PGAdminResult {
		if tunnel.LocalPGHost(req.Socket) {
			return local(ctx, req)
		}
		return pgRemoteAdmin(ctx, pgLocator{root: root}, req)
	}
}

func pgRemoteAdmin(ctx context.Context, l pgLocator, req PGAdminRequest) PGAdminResult {
	where := net2(req.Socket, req.Port)
	b, ok := l.behindPooler(req.Socket, req.Port)
	if !ok {
		return PGAdminResult{Error: fmt.Sprintf("Postgres on %s is not one this server's PgBouncer sends to, "+
			"so the agent will not sign in there", where)}
	}
	logins, missing := l.remoteLogins(b, req)
	if len(logins) == 0 {
		return PGAdminResult{Error: fmt.Sprintf("Postgres is on %s, and the agent found no password to sign in there with: %s. "+
			"Put a password itself (not an md5 or SCRAM hash) for an account that may create roles in %s, "+
			"or install the Argus agent on %s", where, strings.Join(missing, "; "), pgAuthWhere(b), req.Socket)}
	}
	var failed []string
	for _, lg := range logins {
		cfg, err := wire.ParseConfig("sslmode=prefer")
		if err != nil {
			return PGAdminResult{Error: err.Error()}
		}
		cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.Database = req.Socket, uint16(req.Port), lg.User, lg.Password, req.Database
		cfg.ConnectTimeout = pgRemoteWait
		cfg.RuntimeParams = map[string]string{"application_name": "argus-admin"}
		conn, err := pgConnectConfig(ctx, cfg)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s (from %s): %s", lg.User, lg.From, pgSignInWhy(err)))
			continue
		}
		res := RunPGAdmin(ctx, conn, req)
		conn.Close(context.WithoutCancel(ctx))
		// Only a listing moves on to the next account when this one may
		// not: anything that changes Postgres stops where it failed.
		if res.OK || req.Op != "discover-databases" || !pgNotAllowed(res) {
			return res
		}
		failed = append(failed, fmt.Sprintf("%s (from %s): signed in, but may not do this: %s", lg.User, lg.From, res.Error))
	}
	return PGAdminResult{Error: fmt.Sprintf("could not sign in to Postgres on %s with any password PgBouncer has here. Tried %s",
		where, strings.Join(append(failed, missing...), "; "))}
}

// pgRemoteLogin is one user and password to try, and where it was read.
type pgRemoteLogin struct {
	User, Password, From string
}

// remoteLogins are the accounts the agent may sign in to a Postgres on
// another server with, best first: the admin account asked for, then
// PgBouncer's own auth_user, then the user= a [databases] line for that
// server names, then every other account in userlist.txt. Only a password
// itself counts; a hash cannot sign in. missing says why each account that
// was looked for has none.
func (l pgLocator) remoteLogins(b pgBouncerSeen, req PGAdminRequest) (out []pgRemoteLogin, missing []string) {
	want := req.AdminUser
	if want == "" {
		want = "postgres"
	}
	users := map[string]string{}
	var order []string
	if b.AuthFile != "" {
		users = l.userlistOrdered(b.AuthFile, &order)
	}
	seen := map[string]bool{}
	add := func(user, pw, from string) {
		if user == "" || seen[user] {
			return
		}
		switch {
		case pw == "":
			return
		case pgHashedSecret(pw):
			seen[user] = true
			missing = append(missing, fmt.Sprintf("%s has only a hash of %s's password", from, user))
			return
		}
		seen[user] = true
		out = append(out, pgRemoteLogin{User: user, Password: pw, From: from})
	}
	fromList := func(user string) {
		if pw, ok := users[user]; ok {
			add(user, pw, b.AuthFile)
		}
	}
	fromList(want)
	if !seen[want] {
		if b.AuthFile == "" {
			missing = append(missing, b.Config+" names no auth_file")
		} else if _, ok := users[want]; !ok {
			missing = append(missing, fmt.Sprintf("%s has no password for %s", b.AuthFile, want))
		}
		seen[want] = true
	}
	fromList(b.AuthUser)
	for _, lg := range b.Logins {
		if strings.EqualFold(lg.Host, req.Socket) && lg.Port == req.Port {
			if lg.Password != "" {
				add(lg.User, lg.Password, b.Config)
			} else {
				fromList(lg.User)
			}
		}
	}
	for _, u := range order {
		fromList(u)
	}
	return out, missing
}

// pgAuthWhere is the file a password belongs in.
func pgAuthWhere(b pgBouncerSeen) string {
	if b.AuthFile != "" {
		return b.AuthFile
	}
	return b.Config
}

// pgSignInWhy is the readable end of a connection error.
func pgSignInWhy(err error) string {
	var pe *wire.Error
	if errors.As(err, &pe) {
		return pe.Message
	}
	e := err.Error()
	if i := strings.LastIndex(e, "): "); i >= 0 {
		e = e[i+3:]
	}
	return e
}

// pgNotAllowed says an operation failed for want of a privilege, so another
// account may do it.
func pgNotAllowed(r PGAdminResult) bool {
	return r.Code == "42501" || strings.Contains(r.Error, "permission denied") || strings.Contains(r.Error, "must have")
}

var md5Secret = regexp.MustCompile(`^md5[0-9a-f]{32}$`)

// pgHashedSecret says a userlist.txt entry is a stored hash, not a password.
func pgHashedSecret(s string) bool {
	return md5Secret.MatchString(s) || strings.HasPrefix(s, "SCRAM-SHA-256$")
}

// userlist reads PgBouncer's auth_file: one "user" "password" a line, a
// doubled quote inside for a quote.
func (l pgLocator) userlist(path string) map[string]string {
	return l.userlistOrdered(path, nil)
}

// userlistOrdered is userlist, also keeping the users in the file's order.
func (l pgLocator) userlistOrdered(path string, order *[]string) map[string]string {
	out := map[string]string{}
	b, err := l.read(path, 1<<20)
	if err != nil {
		return out
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := quotedFields(sc.Text())
		if len(f) >= 2 {
			if _, dup := out[f[0]]; !dup && order != nil {
				*order = append(*order, f[0])
			}
			out[f[0]] = f[1]
		}
	}
	return out
}

func quotedFields(line string) []string {
	var out []string
	for i := 0; i < len(line); i++ {
		if line[i] != '"' {
			continue
		}
		var b strings.Builder
		j := i + 1
		for ; j < len(line); j++ {
			if line[j] == '"' {
				if j+1 < len(line) && line[j+1] == '"' {
					b.WriteByte('"')
					j++
					continue
				}
				break
			}
			b.WriteByte(line[j])
		}
		out = append(out, b.String())
		i = j
	}
	return out
}

// authFile is a pgbouncer.ini's auth_file, relative to the ini.
func authFile(ini, v string) string {
	if v == "" || filepath.IsAbs(v) {
		return v
	}
	return filepath.Join(filepath.Dir(ini), v)
}
