package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
	"github.com/jackc/pgx/v5/pgconn"
)

/*
The helper's half of "which machines can connect" to a Postgres: it runs as
the server's postgres account, which owns pg_hba.conf and may ALTER SYSTEM.
It reads listen_addresses and pg_hba.conf, and changes them: Argus's own
pg_hba.conf lines (ending "# argus") are replaced, never anyone else's, and a
file Postgres would not load is put back as it was before it is reloaded.
The firewall and a restart are the agent's half (pgnetwork.go), as root.
*/

// PGHBAState is what pg_hba.conf and the settings say about connections from
// other machines.
type PGHBAState struct {
	Listen  string `json:"listen"`
	Port    int    `json:"port"`
	HBAFile string `json:"hbaFile"`
	// Method is what Argus's lines ask for: scram-sha-256, or md5 while
	// passwords are stored that way (md5 also takes SCRAM).
	Method string `json:"method"`
	SSL    bool   `json:"ssl"`
	// PendingRestart says listen_addresses was changed and Postgres must be
	// restarted to listen as it says.
	PendingRestart bool `json:"pendingRestart"`
	// Mine are Argus's lines; Others the server's own host lines for
	// addresses off this server.
	Mine   []string `json:"mine,omitempty"`
	Others []string `json:"others,omitempty"`
	// Trust are host lines that let a machine off this server in with no
	// password; OpenToAll says a line not Argus's lets every address sign in.
	Trust     []string `json:"trust,omitempty"`
	OpenToAll bool     `json:"openToAll,omitempty"`
	// PID is the postmaster's, for the agent to find its service by.
	PID     int    `json:"pid,omitempty"`
	Clients int    `json:"clients"`
	Remote  int    `json:"remote"`
	Version string `json:"version,omitempty"`
}

var hbaHostTypes = map[string]bool{"host": true, "hostssl": true, "hostnossl": true, "hostgssenc": true, "hostnogssenc": true}

// hbaLine is one host line's address and method, or ok false for any other
// line.
func hbaLine(line string) (addr, method string, ok bool) {
	body := line
	if i := strings.IndexByte(body, '#'); i >= 0 {
		body = body[:i]
	}
	f := strings.Fields(body)
	if len(f) < 5 || !hbaHostTypes[f[0]] {
		return "", "", false
	}
	addr, method = f[3], f[4]
	// An address with its mask in a field of its own.
	if !strings.Contains(addr, "/") && len(f) >= 6 {
		if _, err := netip.ParseAddr(f[4]); err == nil {
			if a, err := netip.ParseAddr(addr); err == nil {
				if m, err := netip.ParseAddr(f[4]); err == nil {
					bits := 0
					for _, b := range m.AsSlice() {
						for ; b != 0; b <<= 1 {
							bits += int(b >> 7)
						}
					}
					addr = netip.PrefixFrom(a, bits).String()
				}
			}
			method = f[5]
		}
	}
	return addr, method, true
}

// hbaLoopback says a line's address is only this server.
func hbaLoopback(addr string) bool {
	if addr == "localhost" || addr == "samehost" {
		return true
	}
	if p, err := netip.ParsePrefix(addr); err == nil {
		return p.Addr().IsLoopback() && p.Bits() >= 8
	}
	if a, err := netip.ParseAddr(addr); err == nil {
		return a.IsLoopback()
	}
	return false
}

func hbaEveryone(addr string) bool {
	switch addr {
	case "all", "0.0.0.0/0", "::/0", "0.0.0.0/0.0.0.0":
		return true
	}
	return false
}

// isArgusLine says the line is one Argus wrote.
func isArgusLine(line string) bool {
	return strings.HasSuffix(strings.TrimSpace(line), tunnel.PGHBAMarker)
}

// readHBA fills st from a pg_hba.conf's text.
func readHBA(text string, st *PGHBAState) {
	for _, line := range strings.Split(text, "\n") {
		addr, method, ok := hbaLine(line)
		if !ok {
			continue
		}
		t := strings.Join(strings.Fields(line), " ")
		if isArgusLine(line) {
			st.Mine = append(st.Mine, t)
			continue
		}
		if hbaLoopback(addr) {
			continue
		}
		st.Others = append(st.Others, t)
		if method == "trust" {
			st.Trust = append(st.Trust, t)
		}
		if hbaEveryone(addr) && method != "reject" {
			st.OpenToAll = true
		}
	}
}

// rewriteHBA is text without Argus's lines and with add at the end.
func rewriteHBA(text string, add []string) string {
	var keep []string
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if !isArgusLine(line) {
			keep = append(keep, line)
		}
	}
	for len(keep) > 0 && strings.TrimSpace(keep[len(keep)-1]) == "" {
		keep = keep[:len(keep)-1]
	}
	if len(add) > 0 {
		// The heading ends with the marker too, so it goes with the lines.
		keep = append(keep, "", "# Which machines can connect, set in Argus, which changes only the lines that end with "+tunnel.PGHBAMarker)
		keep = append(keep, add...)
	}
	return strings.Join(keep, "\n") + "\n"
}

func showText(ctx context.Context, conn *pgconn.PgConn, sql string) (string, error) {
	r := conn.ExecParams(ctx, sql, nil, nil, nil, nil).Read()
	if r.Err != nil {
		return "", r.Err
	}
	if len(r.Rows) == 0 || len(r.Rows[0]) == 0 {
		return "", nil
	}
	return string(r.Rows[0][0]), nil
}

func pgHBAState(ctx context.Context, conn *pgconn.PgConn) (PGHBAState, error) {
	var st PGHBAState
	var err error
	q := func(sql string) string {
		if err != nil {
			return ""
		}
		var v string
		v, err = showText(ctx, conn, sql)
		return v
	}
	st.Listen = q("SHOW listen_addresses")
	st.Port, _ = strconv.Atoi(q("SHOW port"))
	st.HBAFile = q("SHOW hba_file")
	enc := q("SHOW password_encryption")
	st.SSL = q("SHOW ssl") == "on"
	st.PendingRestart = q("SELECT pending_restart::text FROM pg_settings WHERE name = 'listen_addresses'") == "true"
	data := q("SHOW data_directory")
	st.Version = q("SHOW server_version")
	st.Clients, _ = strconv.Atoi(q("SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend'"))
	st.Remote, _ = strconv.Atoi(q("SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend' " +
		"AND client_addr IS NOT NULL AND NOT client_addr << '127.0.0.0/8'::inet AND client_addr <> '::1'::inet"))
	if err != nil {
		return st, err
	}
	st.Method = "scram-sha-256"
	if enc != "scram-sha-256" {
		st.Method = "md5"
	}
	b, rerr := os.ReadFile(st.HBAFile)
	if rerr != nil {
		return st, fmt.Errorf("could not read %s: %w", st.HBAFile, rerr)
	}
	readHBA(string(b), &st)
	if pid, err := os.ReadFile(filepath.Join(data, "postmaster.pid")); err == nil {
		st.PID, _ = strconv.Atoi(strings.TrimSpace(strings.SplitN(string(pid), "\n", 2)[0]))
	}
	return st, nil
}

// pgNetworkHelper answers network and network.set in the helper.
func pgNetworkHelper(ctx context.Context, conn *pgconn.PgConn, req PGAdminRequest) PGAdminResult {
	op := req.Admin
	if op.Kind == tunnel.AdminNetworkSet {
		if err := tunnel.ValidatePGNetwork(op); err != nil {
			return PGAdminResult{Error: err.Error()}
		}
		if op.Preview {
			return PGAdminResult{OK: true, SQL: tunnel.PGNetworkText(*op)}
		}
		if err := pgNetworkSet(ctx, conn, *op); err != nil {
			return PGAdminResult{Error: err.Error(), SQL: tunnel.PGNetworkText(*op)}
		}
	}
	st, err := pgHBAState(ctx, conn)
	if err != nil {
		return PGAdminResult{Error: err.Error()}
	}
	b, _ := json.Marshal(st)
	return PGAdminResult{OK: true, Data: b}
}

// pgNetworkSet writes pg_hba.conf and listen_addresses, and reloads.
func pgNetworkSet(ctx context.Context, conn *pgconn.PgConn, op tunnel.PGAdminOp) error {
	st, err := pgHBAState(ctx, conn)
	if err != nil {
		return err
	}
	if op.Reach == tunnel.ReachAny && len(st.Trust) > 0 {
		return fmt.Errorf("pg_hba.conf lets machines in with no password (%s); change that line first", strings.Join(st.Trust, "; "))
	}
	info, err := os.Stat(st.HBAFile)
	if err != nil {
		return err
	}
	old, err := os.ReadFile(st.HBAFile)
	if err != nil {
		return err
	}
	next := rewriteHBA(string(old), tunnel.PGHBALines(op.Reach, op.Sources, st.Method))
	write := func(text string) error {
		tmp := st.HBAFile + ".argus-new"
		if err := os.WriteFile(tmp, []byte(text), info.Mode().Perm()); err != nil {
			return err
		}
		if err := chownLike(tmp, info); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		return os.Rename(tmp, st.HBAFile)
	}
	if next != string(old) {
		if err := write(next); err != nil {
			return fmt.Errorf("could not write %s: %w", st.HBAFile, err)
		}
		// pg_hba_file_rules reads the file as it is on disk: a line Postgres
		// would not load puts the old file back before anything reloads.
		r := conn.ExecParams(ctx, "SELECT line_number::text || ': ' || error FROM pg_hba_file_rules WHERE error IS NOT NULL", nil, nil, nil, nil).Read()
		if r.Err != nil || len(r.Rows) > 0 {
			_ = write(string(old))
			if r.Err != nil {
				return fmt.Errorf("could not check %s: %w", st.HBAFile, r.Err)
			}
			var errs []string
			for _, row := range r.Rows {
				errs = append(errs, string(row[0]))
			}
			return fmt.Errorf("Postgres would not load %s (%s); it was put back as it was", st.HBAFile, strings.Join(errs, "; "))
		}
	}
	if _, err := conn.Exec(ctx, "ALTER SYSTEM SET listen_addresses = "+quoteLiteral(tunnel.PGListenFor(op))).ReadAll(); err != nil {
		if next != string(old) {
			_ = write(string(old))
		}
		return err
	}
	if _, err := conn.Exec(ctx, "SELECT pg_reload_conf()").ReadAll(); err != nil {
		return err
	}
	// The reload is a signal; pending_restart is right once it is read.
	time.Sleep(300 * time.Millisecond)
	return nil
}

var errNotLocalPG = errors.New("this Postgres is on another server; change who can connect there, on its own agent")
