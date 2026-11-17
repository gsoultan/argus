package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
The primary's half of a streaming replica, through the helper (see
tunnel/pgreplica.go):

  - replica-check reads what a replica needs from the primary: wal_level,
    free WAL senders and slots, listen_addresses, and whether pg_hba.conf
    lets the replica's address in for replication as its role. It edits
    nothing; what is missing comes back in words, with the line to add.
  - replica-prepare makes the role (REPLICATION LOGIN, nothing else) with the
    verifier it is given, and the physical slot. Again, it sets the password
    if the role is Argus's from an earlier try.
  - replica-allow adds the replica's pg_hba.conf line and reloads; see below.
  - replica-drop removes all three, refusing while the replica is still
    streaming.
  - replica-status reads how far behind the replica is and how much WAL its
    slot keeps, for the lag watch. It edits nothing.

A role or slot of the same name that Argus did not make is never touched.
*/

// replicaMarker begins the comment on a replication role Argus made. It
// starts with roleMarker, so the owner-level sweep leaves the role alone.
const replicaMarker = roleMarker + " replica by="

func pgReplicaOp(ctx context.Context, conn *pgconn.PgConn, req PGAdminRequest) PGAdminResult {
	version := conn.ParameterStatus("server_version")
	if req.Replica == nil {
		return PGAdminResult{Error: "no replica named", Version: version}
	}
	r := *req.Replica
	if err := r.Check(); err != nil {
		return PGAdminResult{Error: err.Error(), Version: version}
	}
	var (
		data any
		err  error
	)
	switch req.Op {
	case "replica-check":
		data, err = replicaCheck(ctx, conn, r)
	case "replica-prepare":
		data, err = replicaPrepare(ctx, conn, r)
	case "replica-allow":
		data, err = replicaAllow(ctx, conn, r)
	case "replica-drop":
		data, err = replicaDrop(ctx, conn, r)
	case "replica-status":
		data, err = replicaStatus(ctx, conn, r)
	default:
		return PGAdminResult{Error: "unknown operation " + req.Op, Version: version}
	}
	if err != nil {
		return PGAdminResult{Error: err.Error(), Version: version}
	}
	b, _ := json.Marshal(data)
	return PGAdminResult{OK: true, Version: version, Data: b}
}

// row runs a query with text parameters and returns its one row, or nil.
func row(ctx context.Context, conn *pgconn.PgConn, sql string, args ...string) ([][]byte, error) {
	params := make([][]byte, len(args))
	for i, a := range args {
		params[i] = []byte(a)
	}
	res := conn.ExecParams(ctx, sql, params, nil, nil, nil).Read()
	if res.Err != nil {
		return nil, res.Err
	}
	if len(res.Rows) == 0 {
		return nil, nil
	}
	return res.Rows[0], nil
}

func replicaCheck(ctx context.Context, conn *pgconn.PgConn, r tunnel.PGReplica) (tunnel.PGReplicaCheck, error) {
	c := tunnel.PGReplicaCheck{Role: r.Role(), Slot: r.Slot()}
	v, err := row(ctx, conn, `SELECT current_setting('server_version'), current_setting('server_version_num'),
		current_setting('wal_level'), current_setting('max_wal_senders'),
		(SELECT count(*) FROM pg_stat_replication),
		current_setting('max_replication_slots'), (SELECT count(*) FROM pg_replication_slots),
		current_setting('listen_addresses'), current_setting('ssl'),
		EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1),
		EXISTS (SELECT 1 FROM pg_replication_slots WHERE slot_name = $2),
		EXISTS (SELECT 1 FROM pg_replication_slots WHERE slot_name = $2 AND active)`, r.Role(), r.Slot())
	if err != nil {
		return c, err
	}
	num := func(b []byte) int { n, _ := strconv.Atoi(string(b)); return n }
	c.Version, c.VersionNum, c.WalLevel = string(v[0]), num(v[1]), string(v[2])
	c.MaxSenders, c.SendersInUse, c.MaxSlots, c.SlotsInUse = num(v[3]), num(v[4]), num(v[5]), num(v[6])
	c.Listen, c.SSL = string(v[7]), string(v[8]) == "on"
	c.RoleExists, c.SlotExists, c.SlotActive = string(v[9]) == "t", string(v[10]) == "t", string(v[11]) == "t"

	restart := func(s string) {
		c.Missing = append(c.Missing, s)
		c.Restart = true
	}
	if c.WalLevel != "replica" && c.WalLevel != "logical" {
		restart("wal_level is " + c.WalLevel + "; set it to replica in postgresql.conf and restart")
	}
	// The copy streams WAL beside the files, two senders; the replica then
	// keeps one.
	if c.MaxSenders-c.SendersInUse < 2 {
		restart(fmt.Sprintf("max_wal_senders is %d with %d in use; raise it to %d and restart", c.MaxSenders, c.SendersInUse, c.SendersInUse+4))
	}
	// A slot from an earlier try is already counted in use.
	if !c.SlotExists && c.MaxSlots-c.SlotsInUse < 1 {
		restart(fmt.Sprintf("max_replication_slots is %d with %d in use; raise it to %d and restart", c.MaxSlots, c.SlotsInUse, c.SlotsInUse+2))
	}
	if localOnly(c.Listen) {
		restart("listen_addresses is '" + c.Listen + "', so other servers cannot connect; set it to '*' or to this server's address and restart")
	}

	kind := "host"
	if c.SSL {
		kind = "hostssl"
	}
	if r.Address != "" {
		a, _ := netip.ParseAddr(r.Address)
		bits := 32
		if a.Is6() && !a.Is4In6() {
			bits = 128
		}
		c.HBALine = fmt.Sprintf("%s replication %s %s/%d scram-sha-256", kind, r.Role(), a.Unmap(), bits)
		hba, ok, err := hbaAllows(ctx, conn, r.Role(), a.Unmap())
		if err != nil {
			return c, err
		}
		c.HBA, c.HBAOK = hba, ok
		if !ok {
			msg := "pg_hba.conf does not let " + r.Address + " replicate as " + r.Role()
			if hba != "" {
				msg += " (" + hba + " matches first)"
			}
			c.Missing = append(c.Missing, msg+"; add this line above any line that matches it, then reload: "+c.HBALine)
		}
	}
	if c.SlotActive {
		c.Missing = append(c.Missing, "slot "+r.Slot()+" is in use by a replica already streaming")
	}
	return c, nil
}

// localOnly is whether listen_addresses keeps other servers out.
func localOnly(listen string) bool {
	for _, a := range strings.Split(listen, ",") {
		switch strings.TrimSpace(a) {
		case "", "localhost", "127.0.0.1", "::1":
		default:
			return false
		}
	}
	return true
}

// hbaAllows walks pg_hba.conf as Postgres does, first match wins, for a
// replication connection from addr as role. It returns the line that
// matched, if any, and whether that line lets the replica in with a password.
// A line it cannot judge from here (a host name, samenet, a group the role is
// not in yet) is passed over, as it would not match the new role.
func hbaAllows(ctx context.Context, conn *pgconn.PgConn, role string, addr netip.Addr) (string, bool, error) {
	res := conn.ExecParams(ctx, `SELECT line_number, type, array_to_string(database, ','), array_to_string(user_name, ','),
		coalesce(address, ''), coalesce(netmask, ''), coalesce(auth_method, '')
		FROM pg_hba_file_rules WHERE error IS NULL ORDER BY line_number`, nil, nil, nil, nil).Read()
	if res.Err != nil {
		return "", false, res.Err
	}
	for _, r := range res.Rows {
		typ, dbs, users, address, mask, method := string(r[1]), string(r[2]), string(r[3]), string(r[4]), string(r[5]), string(r[6])
		if typ != "host" && typ != "hostssl" && typ != "hostnossl" {
			continue
		}
		if !hbaHas(dbs, "replication") {
			continue
		}
		if !hbaHas(users, "all") && !hbaHas(users, role) {
			continue
		}
		if !addrMatches(address, mask, addr) {
			continue
		}
		line := fmt.Sprintf("line %s: %s replication %s %s", r[0], typ, users, address)
		if mask != "" {
			line += " " + mask
		}
		line += " " + method
		switch method {
		case "scram-sha-256", "md5", "trust", "password":
			return line, true, nil
		}
		return line, false, nil
	}
	return "", false, nil
}

func hbaHas(list, item string) bool {
	for _, s := range strings.Split(list, ",") {
		if s == item {
			return true
		}
	}
	return false
}

// addrMatches is whether a pg_hba.conf address column covers addr.
func addrMatches(address, mask string, addr netip.Addr) bool {
	if address == "all" {
		return true
	}
	if mask != "" {
		a, err1 := netip.ParseAddr(address)
		m, err2 := netip.ParseAddr(mask)
		if err1 != nil || err2 != nil || a.BitLen() != addr.BitLen() {
			return false
		}
		ab, mb, xb := a.AsSlice(), m.AsSlice(), addr.AsSlice()
		for i := range ab {
			if ab[i]&mb[i] != xb[i]&mb[i] {
				return false
			}
		}
		return true
	}
	if p, err := netip.ParsePrefix(address); err == nil {
		return p.Contains(addr)
	}
	if a, err := netip.ParseAddr(address); err == nil {
		return a == addr
	}
	return false
}

// replicaRoleState says whether the role exists and whether Argus made it.
func replicaRoleState(ctx context.Context, conn *pgconn.PgConn, role string) (exists, ours bool, err error) {
	v, err := row(ctx, conn, `SELECT coalesce(shobj_description(oid, 'pg_authid'), ''), rolsuper OR rolcreaterole OR rolcreatedb OR rolbypassrls
		FROM pg_roles WHERE rolname = $1`, role)
	if err != nil || v == nil {
		return false, false, err
	}
	return true, strings.HasPrefix(string(v[0]), replicaMarker) && string(v[1]) == "f", nil
}

// PGReplicaPrepared answers replica-prepare.
type PGReplicaPrepared struct {
	Role        string `json:"role"`
	Slot        string `json:"slot"`
	RoleCreated bool   `json:"roleCreated"`
	SlotCreated bool   `json:"slotCreated"`
}

func replicaPrepare(ctx context.Context, conn *pgconn.PgConn, r tunnel.PGReplica) (PGReplicaPrepared, error) {
	out := PGReplicaPrepared{Role: r.Role(), Slot: r.Slot()}
	if !scramPattern.MatchString(r.Verifier) {
		return out, fmt.Errorf("the replication password is not a SCRAM verifier")
	}
	role := pgx(r.Role())
	exists, ours, err := replicaRoleState(ctx, conn, r.Role())
	if err != nil {
		return out, err
	}
	switch {
	case exists && !ours:
		return out, fmt.Errorf("a role named %s exists and Argus did not make it; Argus leaves it alone", r.Role())
	case exists:
		if err := conn.Exec(ctx, "ALTER ROLE "+role+" WITH REPLICATION LOGIN NOINHERIT CONNECTION LIMIT 3 PASSWORD "+quoteLiteral(r.Verifier)).Close(); err != nil {
			return out, err
		}
	default:
		if err := conn.Exec(ctx, "CREATE ROLE "+role+" WITH REPLICATION LOGIN NOINHERIT CONNECTION LIMIT 3 PASSWORD "+quoteLiteral(r.Verifier)+
			"; COMMENT ON ROLE "+role+" IS "+quoteLiteral(replicaMarker+r.By)).Close(); err != nil {
			return out, err
		}
		out.RoleCreated = true
	}
	v, err := row(ctx, conn, `SELECT slot_type, active FROM pg_replication_slots WHERE slot_name = $1`, r.Slot())
	if err != nil {
		return out, err
	}
	switch {
	case v != nil && string(v[0]) != "physical":
		return out, fmt.Errorf("a %s slot named %s exists and Argus did not make it; Argus leaves it alone", v[0], r.Slot())
	case v != nil && string(v[1]) == "t":
		return out, fmt.Errorf("slot %s is in use by a replica already streaming", r.Slot())
	case v == nil:
		// Not reserved until the replica first connects, so a clone that
		// never starts does not hold WAL on the primary.
		if _, err := row(ctx, conn, `SELECT pg_create_physical_replication_slot($1, false)`, r.Slot()); err != nil {
			return out, err
		}
		out.SlotCreated = true
	}
	return out, nil
}

// PGReplicaDropped answers replica-drop.
type PGReplicaDropped struct {
	RoleDropped bool `json:"roleDropped"`
	SlotDropped bool `json:"slotDropped"`
	HBARemoved  bool `json:"hbaRemoved"`
}

func replicaDrop(ctx context.Context, conn *pgconn.PgConn, r tunnel.PGReplica) (PGReplicaDropped, error) {
	var out PGReplicaDropped
	v, err := row(ctx, conn, `SELECT slot_type, active FROM pg_replication_slots WHERE slot_name = $1`, r.Slot())
	if err != nil {
		return out, err
	}
	if v != nil {
		if string(v[1]) == "t" {
			return out, fmt.Errorf("slot %s is in use: the replica is still streaming; stop it first", r.Slot())
		}
		if string(v[0]) == "physical" {
			if _, err := row(ctx, conn, `SELECT pg_drop_replication_slot($1)`, r.Slot()); err != nil {
				return out, err
			}
			out.SlotDropped = true
		}
	}
	exists, ours, err := replicaRoleState(ctx, conn, r.Role())
	if err != nil {
		return out, err
	}
	if exists && ours {
		if _, err := row(ctx, conn, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = $1`, r.Role()); err != nil {
			return out, err
		}
		if err := conn.Exec(ctx, "DROP ROLE "+pgx(r.Role())).Close(); err != nil {
			return out, err
		}
		out.RoleDropped = true
	}
	file, err := row(ctx, conn, `SELECT current_setting('hba_file')`)
	if err != nil {
		return out, err
	}
	if b, err := hbaFiles.read(string(file[0])); err == nil && strings.Contains(string(b), hbaMark+r.Name) {
		if err := hbaEdit(ctx, conn, string(file[0]), func(s string) string { return hbaWithout(s, r.Name) }, func() { out.HBARemoved = true }); err != nil {
			return out, err
		}
	}
	return out, nil
}

/*
replica-allow: the one edit Argus makes to a primary's configuration, chosen
by the owner over check-only. It adds a single pg_hba.conf line for the
replica's address and role, marked as Argus's, above every other rule so no
earlier line can shadow it, and reloads. It never touches postgresql.conf and
never restarts the server: a setting that needs a restart stays the owner's
to make. The file is checked by Postgres before the reload, and put back as
it was if Postgres would reject it.
*/

// hbaMark ends every pg_hba.conf line Argus adds, naming the replica.
const hbaMark = "# argus-replica:"

// hbaFiles reads and writes pg_hba.conf. The helper runs as the postgres
// account, which owns the file; tests swap it.
var hbaFiles = struct {
	read  func(string) ([]byte, error)
	write func(string, []byte) error
}{read: os.ReadFile, write: func(p string, b []byte) error {
	st, err := os.Stat(p)
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, st.Mode().Perm())
}}

// hbaWithReplica returns content with the line for name put before the first
// rule, and any earlier line for name taken out.
func hbaWithReplica(content, name, line string) string {
	lines := strings.Split(hbaWithout(content, name), "\n")
	mark := line + " " + hbaMark + name
	at := len(lines)
	for i, l := range lines {
		if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, "#") {
			at = i
			break
		}
	}
	out := append([]string{}, lines[:at]...)
	out = append(out, mark)
	return strings.Join(append(out, lines[at:]...), "\n")
}

// hbaWithout returns content with Argus's line for name taken out.
func hbaWithout(content, name string) string {
	lines := strings.Split(content, "\n")
	out := lines[:0:0]
	for _, l := range lines {
		if strings.HasSuffix(strings.TrimRight(l, " \t\r"), hbaMark+name) {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// PGReplicaAllowed answers replica-allow.
type PGReplicaAllowed struct {
	File  string `json:"file"`
	Line  string `json:"line"`
	Added bool   `json:"added"`
}

func replicaAllow(ctx context.Context, conn *pgconn.PgConn, r tunnel.PGReplica) (PGReplicaAllowed, error) {
	var out PGReplicaAllowed
	if r.Address == "" {
		return out, fmt.Errorf("name the replica's address")
	}
	c, err := replicaCheck(ctx, conn, r)
	if err != nil {
		return out, err
	}
	out.Line = c.HBALine
	if c.HBAOK {
		return out, nil // a line already lets it in
	}
	v, err := row(ctx, conn, `SELECT current_setting('hba_file')`)
	if err != nil {
		return out, err
	}
	out.File = string(v[0])
	return out, hbaEdit(ctx, conn, out.File, func(s string) string { return hbaWithReplica(s, r.Name, c.HBALine) }, func() { out.Added = true })
}

// hbaEdit changes pg_hba.conf, has Postgres check it, and reloads; on any
// error in the new file it puts the old one back.
func hbaEdit(ctx context.Context, conn *pgconn.PgConn, file string, change func(string) string, done func()) error {
	before, err := hbaFiles.read(file)
	if err != nil {
		return fmt.Errorf("reading %s: %w", file, err)
	}
	after := change(string(before))
	if after == string(before) {
		return nil
	}
	if err := hbaFiles.write(file, []byte(after)); err != nil {
		return fmt.Errorf("writing %s: %w", file, err)
	}
	bad, err := row(ctx, conn, `SELECT line_number, error FROM pg_hba_file_rules WHERE error IS NOT NULL ORDER BY line_number LIMIT 1`)
	if err == nil && bad != nil {
		err = fmt.Errorf("Postgres would reject %s at line %s: %s", file, bad[0], bad[1])
	}
	if err != nil {
		if werr := hbaFiles.write(file, before); werr != nil {
			return fmt.Errorf("%v; and putting the file back failed: %v", err, werr)
		}
		return err
	}
	if _, err := row(ctx, conn, `SELECT pg_reload_conf()`); err != nil {
		return err
	}
	done()
	return nil
}

// replicaStatus reads the replica's WAL sender and slot on the primary.
func replicaStatus(ctx context.Context, conn *pgconn.PgConn, r tunnel.PGReplica) (tunnel.PGReplicaStatus, error) {
	st := tunnel.PGReplicaStatus{LagBytes: -1}
	v, err := row(ctx, conn, `SELECT
		EXISTS (SELECT 1 FROM pg_replication_slots WHERE slot_name = $2),
		coalesce((SELECT active FROM pg_replication_slots WHERE slot_name = $2), false),
		coalesce((SELECT pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)::bigint
			FROM pg_replication_slots WHERE slot_name = $2 AND restart_lsn IS NOT NULL), 0),
		coalesce(s.state, ''), coalesce(host(s.client_addr), ''),
		extract(epoch FROM s.replay_lag)::float8,
		coalesce(pg_wal_lsn_diff(pg_current_wal_lsn(), s.replay_lsn)::bigint, -1)
		FROM (SELECT 1) one LEFT JOIN LATERAL (
			SELECT state, client_addr, replay_lag, replay_lsn FROM pg_stat_replication
			WHERE usename = $1 ORDER BY backend_start DESC LIMIT 1) s ON true`, r.Role(), r.Slot())
	if err != nil {
		return st, err
	}
	n := func(b []byte) int64 { x, _ := strconv.ParseInt(string(b), 10, 64); return x }
	st.SlotExists, st.SlotActive, st.RetainedBytes = string(v[0]) == "t", string(v[1]) == "t", n(v[2])
	st.State, st.Client = string(v[3]), string(v[4])
	if v[5] != nil {
		f, _ := strconv.ParseFloat(string(v[5]), 64)
		st.ReplayLagS = &f
	}
	st.LagBytes = n(v[6])
	st.Streaming = st.State == "streaming"
	return st, nil
}
