package agent

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

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
	user := req.AdminUser
	if user == "" {
		user = "postgres"
	}
	if b.AuthFile == "" {
		return PGAdminResult{Error: fmt.Sprintf("Postgres is on %s, and %s names no auth_file to find %s's password in",
			where, b.Config, user)}
	}
	users := l.userlist(b.AuthFile)
	pw, found := users[user]
	switch {
	case !found:
		return PGAdminResult{Error: fmt.Sprintf("Postgres is on %s, and %s has no password for %s; "+
			"add it there, or install the Argus agent on %s", where, b.AuthFile, user, req.Socket)}
	case pgHashedSecret(pw):
		return PGAdminResult{Error: fmt.Sprintf("Postgres is on %s, and %s holds only a hash of %s's password, "+
			"which cannot sign in; put the password itself there, or install the Argus agent on %s",
			where, b.AuthFile, user, req.Socket)}
	}
	cfg, err := pgconn.ParseConfig("sslmode=prefer")
	if err != nil {
		return PGAdminResult{Error: err.Error()}
	}
	cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.Database = req.Socket, uint16(req.Port), user, pw, req.Database
	cfg.ConnectTimeout = pgRemoteWait
	cfg.RuntimeParams = map[string]string{"application_name": "argus-admin"}
	conn, err := pgconn.ConnectConfig(ctx, cfg)
	if err != nil {
		return PGAdminResult{Error: fmt.Sprintf("could not sign in to Postgres as %s on %s with the password in %s: %v",
			user, where, b.AuthFile, err)}
	}
	defer conn.Close(context.WithoutCancel(ctx))
	return RunPGAdmin(ctx, conn, req)
}

var md5Secret = regexp.MustCompile(`^md5[0-9a-f]{32}$`)

// pgHashedSecret says a userlist.txt entry is a stored hash, not a password.
func pgHashedSecret(s string) bool {
	return md5Secret.MatchString(s) || strings.HasPrefix(s, "SCRAM-SHA-256$")
}

// userlist reads PgBouncer's auth_file: one "user" "password" a line, a
// doubled quote inside for a quote.
func (l pgLocator) userlist(path string) map[string]string {
	out := map[string]string{}
	b, err := l.read(path, 1<<20)
	if err != nil {
		return out
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := quotedFields(sc.Text())
		if len(f) >= 2 {
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
