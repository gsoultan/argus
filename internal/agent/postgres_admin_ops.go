package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Extensions and logical replication, through the helper.

Each operation is a fixed statement built from validated fields. The builder
is pure (no database), so the agent can record the redacted statement before
the helper runs the real one; the helper calls the same builder, so what is
recorded is what ran. The only secret is a subscription's password: the
builder returns the statement twice, once to run and once with the password
replaced by [redacted], and only the second ever leaves this process.

The design, with why these are not plain recorded statements:
plans/postgres-extensions-replication/design.md.
*/

// adminMarker begins the comment on every publication and subscription Argus
// created, so the console can say who set it up.
const adminMarker = "argus-managed by="

// maxIdent is Postgres's identifier length.
const maxIdent = 63

var (
	versionPattern = regexp.MustCompile(`^[A-Za-z0-9_.+-]{1,64}$`)
	sslModes       = map[string]bool{"disable": true, "allow": true, "prefer": true, "require": true,
		"verify-ca": true, "verify-full": true}
	publishOps = map[string]bool{"insert": true, "update": true, "delete": true, "truncate": true}
)

// adminSQL is what an operation runs: the statements in order, and the same
// with any password redacted.
type adminSQL struct {
	run      []string
	redacted []string
}

func (a adminSQL) text() string { return strings.Join(a.redacted, ";\n") }

// quoteIdent quotes any identifier. Unlike pgx it accepts names Argus did
// not make (uuid-ossp, a publication called "Orders Pub"); it refuses only
// what Postgres would refuse.
func quoteIdent(name string) (string, error) {
	if name == "" {
		return "", errors.New("a name is required")
	}
	if len(name) > maxIdent {
		return "", fmt.Errorf("%q is longer than %d characters", name, maxIdent)
	}
	if strings.IndexByte(name, 0) >= 0 {
		return "", errors.New("a name cannot contain NUL")
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`, nil
}

// qualified quotes "schema.table". A dot inside a name is written as
// schema."a.b" by the caller; the first dot splits.
func qualified(t string) (string, error) {
	schema, table, ok := strings.Cut(t, ".")
	if !ok || schema == "" || table == "" {
		return "", fmt.Errorf("a table is named schema.table, not %q", t)
	}
	s, err := quoteIdent(schema)
	if err != nil {
		return "", err
	}
	n, err := quoteIdent(table)
	if err != nil {
		return "", err
	}
	return s + "." + n, nil
}

// conninfo writes a libpq connection string from fields. Each value is
// single-quoted with backslash and quote escaped, so a value is a value
// whatever is in it; the whole becomes a SQL literal through quoteLiteral.
func conninfo(c *tunnel.PGConnInfo, redact bool) (string, error) {
	if c == nil {
		return "", errors.New("the publisher's connection is required")
	}
	if strings.TrimSpace(c.Host) == "" || strings.TrimSpace(c.DBName) == "" || strings.TrimSpace(c.User) == "" {
		return "", errors.New("the publisher needs a host, a database and a user")
	}
	if c.Port < 0 || c.Port > 65535 {
		return "", errors.New("the port must be between 1 and 65535")
	}
	if c.SSLMode != "" && !sslModes[c.SSLMode] {
		return "", fmt.Errorf("unknown sslmode %q", c.SSLMode)
	}
	q := func(v string) string {
		v = strings.ReplaceAll(v, `\`, `\\`)
		v = strings.ReplaceAll(v, `'`, `\'`)
		return "'" + v + "'"
	}
	for _, v := range []string{c.Host, c.DBName, c.User, c.Password} {
		if strings.IndexByte(v, 0) >= 0 {
			return "", errors.New("a connection value cannot contain NUL")
		}
	}
	parts := []string{"host=" + q(c.Host)}
	if c.Port != 0 {
		parts = append(parts, "port="+strconv.Itoa(c.Port))
	}
	parts = append(parts, "dbname="+q(c.DBName), "user="+q(c.User))
	if c.Password != "" {
		if redact {
			parts = append(parts, "password=[redacted]")
		} else {
			parts = append(parts, "password="+q(c.Password))
		}
	}
	if c.SSLMode != "" {
		parts = append(parts, "sslmode="+c.SSLMode)
	}
	return strings.Join(parts, " "), nil
}

var conninfoPassword = regexp.MustCompile(`(?i)(\bpassword\s*=\s*)('(?:[^'\\]|\\.)*'|\S+)`)
var uriPassword = regexp.MustCompile(`^(postgres(?:ql)?://[^:/@\s]*):[^@/\s]*@`)

// redactConninfo hides the password in a connection string Postgres holds,
// whichever of libpq's two forms it is in. Anything it cannot parse is not
// shown at all.
func redactConninfo(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if uriPassword.MatchString(s) || strings.HasPrefix(s, "postgres://") || strings.HasPrefix(s, "postgresql://") {
		return uriPassword.ReplaceAllString(s, "$1:[redacted]@")
	}
	return conninfoPassword.ReplaceAllString(s, "${1}[redacted]")
}

// publisherOf summarises a connection string as user@host:port/dbname, for
// the list. Only those four keys are read.
func publisherOf(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "postgres://") || strings.HasPrefix(s, "postgresql://") {
		return redactConninfo(s)
	}
	kv := map[string]string{}
	for i := 0; i < len(s); {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n') {
			i++
		}
		j := i
		for j < len(s) && s[j] != '=' && s[j] != ' ' {
			j++
		}
		if j >= len(s) || s[j] != '=' {
			break
		}
		key := s[i:j]
		j++
		for j < len(s) && s[j] == ' ' {
			j++
		}
		var val strings.Builder
		if j < len(s) && s[j] == '\'' {
			j++
			for j < len(s) && s[j] != '\'' {
				if s[j] == '\\' && j+1 < len(s) {
					j++
				}
				val.WriteByte(s[j])
				j++
			}
			j++
		} else {
			for j < len(s) && s[j] != ' ' {
				if s[j] == '\\' && j+1 < len(s) {
					j++
				}
				val.WriteByte(s[j])
				j++
			}
		}
		kv[key] = val.String()
		i = j
	}
	out := kv["host"]
	if out == "" {
		out = kv["hostaddr"]
	}
	if kv["user"] != "" {
		out = kv["user"] + "@" + out
	}
	if kv["port"] != "" {
		out += ":" + kv["port"]
	}
	if kv["dbname"] != "" {
		out += "/" + kv["dbname"]
	}
	return out
}

// buildAdminSQL turns an operation into its statements. Pure: every check
// that needs the catalogue is the helper's, and Postgres has the last word.
func buildAdminSQL(op *tunnel.PGAdminOp, actor string) (adminSQL, error) {
	if op == nil {
		return adminSQL{}, errors.New("no operation")
	}
	one := func(s string) adminSQL { return adminSQL{run: []string{s}, redacted: []string{s}} }
	comment := func(kind, name string) string {
		return "COMMENT ON " + kind + " " + name + " IS " + quoteLiteral(adminMarker+actor)
	}
	switch op.Kind {
	case tunnel.AdminExtensionList, tunnel.AdminPublicationList, tunnel.AdminSubscriptionList:
		return adminSQL{}, nil

	case tunnel.AdminExtensionCreate:
		n, err := quoteIdent(op.Name)
		if err != nil {
			return adminSQL{}, err
		}
		s := "CREATE EXTENSION IF NOT EXISTS " + n
		if op.Schema != "" {
			sc, err := quoteIdent(op.Schema)
			if err != nil {
				return adminSQL{}, err
			}
			s += " SCHEMA " + sc
		}
		if op.Version != "" {
			if !versionPattern.MatchString(op.Version) {
				return adminSQL{}, fmt.Errorf("%q is not an extension version", op.Version)
			}
			s += " VERSION " + quoteLiteral(op.Version)
		}
		if op.Cascade {
			s += " CASCADE"
		}
		return one(s), nil

	case tunnel.AdminExtensionUpdate:
		n, err := quoteIdent(op.Name)
		if err != nil {
			return adminSQL{}, err
		}
		s := "ALTER EXTENSION " + n + " UPDATE"
		if op.Version != "" {
			if !versionPattern.MatchString(op.Version) {
				return adminSQL{}, fmt.Errorf("%q is not an extension version", op.Version)
			}
			s += " TO " + quoteLiteral(op.Version)
		}
		return one(s), nil

	case tunnel.AdminExtensionDrop:
		n, err := quoteIdent(op.Name)
		if err != nil {
			return adminSQL{}, err
		}
		s := "DROP EXTENSION " + n
		if op.Cascade {
			s += " CASCADE"
		}
		return one(s), nil

	case tunnel.AdminPublicationCreate:
		n, err := quoteIdent(op.Name)
		if err != nil {
			return adminSQL{}, err
		}
		s := "CREATE PUBLICATION " + n
		if op.AllTables {
			s += " FOR ALL TABLES"
		} else {
			list, err := tableList(op.Tables)
			if err != nil {
				return adminSQL{}, err
			}
			s += " FOR TABLE " + list
		}
		if len(op.Publish) > 0 {
			ops, err := publishList(op.Publish)
			if err != nil {
				return adminSQL{}, err
			}
			s += " WITH (publish = " + quoteLiteral(ops) + ")"
		}
		return adminSQL{run: []string{s, comment("PUBLICATION", n)}, redacted: []string{s}}, nil

	case tunnel.AdminPublicationTables:
		n, err := quoteIdent(op.Name)
		if err != nil {
			return adminSQL{}, err
		}
		list, err := tableList(op.Tables)
		if err != nil {
			return adminSQL{}, err
		}
		return one("ALTER PUBLICATION " + n + " SET TABLE " + list), nil

	case tunnel.AdminPublicationOptions:
		n, err := quoteIdent(op.Name)
		if err != nil {
			return adminSQL{}, err
		}
		var set []string
		if len(op.Publish) > 0 {
			ops, err := publishList(op.Publish)
			if err != nil {
				return adminSQL{}, err
			}
			set = append(set, "publish = "+quoteLiteral(ops))
		}
		if op.ViaRoot != nil {
			set = append(set, "publish_via_partition_root = "+strconv.FormatBool(*op.ViaRoot))
		}
		if len(set) == 0 {
			return adminSQL{}, errors.New("nothing to change: choose what to publish")
		}
		return one("ALTER PUBLICATION " + n + " SET (" + strings.Join(set, ", ") + ")"), nil

	case tunnel.AdminPublicationDrop:
		n, err := quoteIdent(op.Name)
		if err != nil {
			return adminSQL{}, err
		}
		return one("DROP PUBLICATION " + n), nil

	case tunnel.AdminSubscriptionCreate:
		n, err := quoteIdent(op.Name)
		if err != nil {
			return adminSQL{}, err
		}
		full, err := conninfo(op.Conn, false)
		if err != nil {
			return adminSQL{}, err
		}
		red, _ := conninfo(op.Conn, true)
		if len(op.Publications) == 0 {
			return adminSQL{}, errors.New("at least one publication is required")
		}
		pubs := make([]string, 0, len(op.Publications))
		for _, p := range op.Publications {
			q, err := quoteIdent(p)
			if err != nil {
				return adminSQL{}, err
			}
			pubs = append(pubs, q)
		}
		var with []string
		flag := func(name string, v *bool) {
			if v != nil {
				with = append(with, name+" = "+strconv.FormatBool(*v))
			}
		}
		flag("connect", op.Connect)
		flag("enabled", op.Enabled)
		flag("create_slot", op.CreateSlot)
		flag("copy_data", op.CopyData)
		if op.SlotName != "" {
			if _, err := quoteIdent(op.SlotName); err != nil {
				return adminSQL{}, err
			}
			with = append(with, "slot_name = "+quoteLiteral(op.SlotName))
		}
		head := "CREATE SUBSCRIPTION " + n + " CONNECTION "
		tail := " PUBLICATION " + strings.Join(pubs, ", ")
		if len(with) > 0 {
			tail += " WITH (" + strings.Join(with, ", ") + ")"
		}
		return adminSQL{
			run:      []string{head + quoteLiteral(full) + tail, comment("SUBSCRIPTION", n)},
			redacted: []string{head + quoteLiteral(red) + tail},
		}, nil

	case tunnel.AdminSubscriptionConnection:
		n, err := quoteIdent(op.Name)
		if err != nil {
			return adminSQL{}, err
		}
		full, err := conninfo(op.Conn, false)
		if err != nil {
			return adminSQL{}, err
		}
		red, _ := conninfo(op.Conn, true)
		head := "ALTER SUBSCRIPTION " + n + " CONNECTION "
		return adminSQL{run: []string{head + quoteLiteral(full)}, redacted: []string{head + quoteLiteral(red)}}, nil

	case tunnel.AdminSubscriptionEnable, tunnel.AdminSubscriptionDisable:
		n, err := quoteIdent(op.Name)
		if err != nil {
			return adminSQL{}, err
		}
		verb := " ENABLE"
		if op.Kind == tunnel.AdminSubscriptionDisable {
			verb = " DISABLE"
		}
		return one("ALTER SUBSCRIPTION " + n + verb), nil

	case tunnel.AdminSubscriptionRefresh:
		n, err := quoteIdent(op.Name)
		if err != nil {
			return adminSQL{}, err
		}
		s := "ALTER SUBSCRIPTION " + n + " REFRESH PUBLICATION"
		if op.CopyData != nil {
			s += " WITH (copy_data = " + strconv.FormatBool(*op.CopyData) + ")"
		}
		return one(s), nil

	case tunnel.AdminSubscriptionPublications:
		n, err := quoteIdent(op.Name)
		if err != nil {
			return adminSQL{}, err
		}
		if len(op.Publications) == 0 {
			return adminSQL{}, errors.New("at least one publication is required")
		}
		pubs := make([]string, 0, len(op.Publications))
		for _, p := range op.Publications {
			q, err := quoteIdent(p)
			if err != nil {
				return adminSQL{}, err
			}
			pubs = append(pubs, q)
		}
		s := "ALTER SUBSCRIPTION " + n + " SET PUBLICATION " + strings.Join(pubs, ", ")
		var with []string
		if op.Refresh != nil {
			with = append(with, "refresh = "+strconv.FormatBool(*op.Refresh))
		}
		if op.CopyData != nil && (op.Refresh == nil || *op.Refresh) {
			with = append(with, "copy_data = "+strconv.FormatBool(*op.CopyData))
		}
		if len(with) > 0 {
			s += " WITH (" + strings.Join(with, ", ") + ")"
		}
		return one(s), nil

	case tunnel.AdminSubscriptionOptions:
		n, err := quoteIdent(op.Name)
		if err != nil {
			return adminSQL{}, err
		}
		var set []string
		if op.SlotName != "" {
			if strings.EqualFold(op.SlotName, "NONE") {
				set = append(set, "slot_name = NONE")
			} else {
				if _, err := quoteIdent(op.SlotName); err != nil {
					return adminSQL{}, err
				}
				set = append(set, "slot_name = "+quoteLiteral(op.SlotName))
			}
		}
		if op.SyncCommit != "" {
			if !syncCommits[op.SyncCommit] {
				return adminSQL{}, fmt.Errorf("%q is not one of off, local, remote_write, on, remote_apply", op.SyncCommit)
			}
			set = append(set, "synchronous_commit = "+quoteLiteral(op.SyncCommit))
		}
		if op.Streaming != "" {
			if !streamings[op.Streaming] {
				return adminSQL{}, fmt.Errorf("%q is not one of off, on, parallel", op.Streaming)
			}
			set = append(set, "streaming = "+quoteLiteral(op.Streaming))
		}
		if op.Binary != nil {
			set = append(set, "binary = "+strconv.FormatBool(*op.Binary))
		}
		if op.DisableOnError != nil {
			set = append(set, "disable_on_error = "+strconv.FormatBool(*op.DisableOnError))
		}
		if len(set) == 0 {
			return adminSQL{}, errors.New("nothing to change")
		}
		return one("ALTER SUBSCRIPTION " + n + " SET (" + strings.Join(set, ", ") + ")"), nil

	case tunnel.AdminSubscriptionDrop:
		n, err := quoteIdent(op.Name)
		if err != nil {
			return adminSQL{}, err
		}
		if op.PublisherGone {
			// A plain DROP contacts the publisher to drop the slot and fails
			// when it cannot. Disabling and forgetting the slot first is
			// the documented way out; the slot is then the publisher's.
			return adminSQL{run: []string{
				"ALTER SUBSCRIPTION " + n + " DISABLE",
				"ALTER SUBSCRIPTION " + n + " SET (slot_name = NONE)",
				"DROP SUBSCRIPTION " + n,
			}, redacted: []string{
				"ALTER SUBSCRIPTION " + n + " DISABLE",
				"ALTER SUBSCRIPTION " + n + " SET (slot_name = NONE)",
				"DROP SUBSCRIPTION " + n,
			}}, nil
		}
		return one("DROP SUBSCRIPTION " + n), nil
	}
	return adminSQL{}, fmt.Errorf("unknown operation %q", op.Kind)
}

var syncCommits = map[string]bool{"off": true, "local": true, "remote_write": true, "on": true, "remote_apply": true}
var streamings = map[string]bool{"off": true, "on": true, "parallel": true}

func tableList(tables []string) (string, error) {
	if len(tables) == 0 {
		return "", errors.New("at least one table is required")
	}
	if len(tables) > 1000 {
		return "", errors.New("more than 1,000 tables; publish all tables instead")
	}
	out := make([]string, 0, len(tables))
	for _, t := range tables {
		q, err := qualified(t)
		if err != nil {
			return "", err
		}
		out = append(out, q)
	}
	return strings.Join(out, ", "), nil
}

func publishList(ops []string) (string, error) {
	seen := map[string]bool{}
	var out []string
	for _, o := range ops {
		o = strings.ToLower(strings.TrimSpace(o))
		if !publishOps[o] {
			return "", fmt.Errorf("%q is not one of insert, update, delete, truncate", o)
		}
		if !seen[o] {
			seen[o] = true
			out = append(out, o)
		}
	}
	return strings.Join(out, ", "), nil
}

/* ── what the helper runs ─────────────────────────────────────────────── */

// PGExtension is one row of extension.list.
type PGExtension struct {
	Name             string `json:"name"`
	InstalledVersion string `json:"installedVersion,omitempty"`
	DefaultVersion   string `json:"defaultVersion,omitempty"`
	// Versions is every version the server can install, newest last as the
	// server lists them.
	Versions    []string `json:"versions,omitempty"`
	Schema      string   `json:"schema,omitempty"`
	Relocatable bool     `json:"relocatable,omitempty"`
	// Trusted says a non-superuser with CREATE on the database may install
	// it; Superuser says it needs one. Both are Postgres's own flags.
	Trusted   bool   `json:"trusted,omitempty"`
	Superuser bool   `json:"superuser,omitempty"`
	Comment   string `json:"comment,omitempty"`
}

// PGPublication is one row of publication.list.
type PGPublication struct {
	Name      string   `json:"name"`
	Owner     string   `json:"owner"`
	AllTables bool     `json:"allTables"`
	Tables    []string `json:"tables,omitempty"`
	Publish   []string `json:"publish"`
	ViaRoot   bool     `json:"viaRoot,omitempty"`
	// SetUpBy is who created it through Argus, from its comment; empty for
	// a publication made outside Argus.
	SetUpBy string `json:"setUpBy,omitempty"`
}

// PGSubscription is one row of subscription.list. Nothing in it is the
// password: Publisher and Conninfo are redacted before they leave the helper.
type PGSubscription struct {
	Name         string   `json:"name"`
	Enabled      bool     `json:"enabled"`
	Publisher    string   `json:"publisher"`
	Conninfo     string   `json:"conninfo"`
	Publications []string `json:"publications"`
	Slot         string   `json:"slot,omitempty"`
	Owner        string   `json:"owner"`
	SetUpBy      string   `json:"setUpBy,omitempty"`
	// From pg_stat_subscription's apply worker, when one runs.
	WorkerPID     int    `json:"workerPid,omitempty"`
	ReceivedLSN   string `json:"receivedLsn,omitempty"`
	LatestEndLSN  string `json:"latestEndLsn,omitempty"`
	LastMessageAt string `json:"lastMessageAt,omitempty"`
	LatestEndAt   string `json:"latestEndAt,omitempty"`
	ApplyLagS     *int64 `json:"applyLagS,omitempty"`
	TablesReady   int    `json:"tablesReady"`
	TablesSyncing int    `json:"tablesSyncing"`
	TablesWaiting int    `json:"tablesWaiting"`
	ApplyErrors   *int64 `json:"applyErrors,omitempty"`
	SyncErrors    *int64 `json:"syncErrors,omitempty"`
	SyncCommit    string `json:"syncCommit,omitempty"`
	// Binary and Streaming are Postgres 14 and later, DisableOnError 15 and
	// later; empty before.
	Binary         string `json:"binary,omitempty"`
	Streaming      string `json:"streaming,omitempty"`
	DisableOnError string `json:"disableOnError,omitempty"`
}

// PGAdminDone answers a mutation: the redacted statement and Postgres's tag.
type PGAdminDone struct {
	SQL string `json:"sql"`
	Tag string `json:"tag,omitempty"`
}

// pgAdminOp runs one administrative operation on conn, a superuser's.
func pgAdminOp(ctx context.Context, conn *pgconn.PgConn, req PGAdminRequest) PGAdminResult {
	op := req.Admin
	if op == nil {
		return PGAdminResult{Error: "no operation"}
	}
	built, err := buildAdminSQL(op, req.Owner)
	if err != nil {
		return PGAdminResult{Error: err.Error()}
	}
	if op.Preview {
		return PGAdminResult{OK: true, SQL: built.text()}
	}
	var (
		data any
	)
	switch op.Kind {
	case tunnel.AdminExtensionList:
		data, err = listExtensions(ctx, conn)
	case tunnel.AdminPublicationList:
		data, err = listPublications(ctx, conn)
	case tunnel.AdminSubscriptionList:
		data, err = listSubscriptions(ctx, conn)
	case tunnel.AdminExtensionCreate:
		// A clear refusal for a name the server does not have, rather than
		// Postgres's "could not open extension control file".
		res := conn.ExecParams(ctx, `SELECT 1 FROM pg_available_extensions WHERE name = $1`,
			[][]byte{[]byte(op.Name)}, nil, nil, nil).Read()
		if res.Err != nil {
			return pgAdminFailed(res.Err, built)
		}
		if len(res.Rows) == 0 {
			return PGAdminResult{Error: "no extension named " + op.Name + " is available on this server", SQL: built.text()}
		}
		fallthrough
	default:
		var tag string
		for _, s := range built.run {
			t, err := conn.Exec(ctx, s).ReadAll()
			if err != nil {
				return pgAdminFailed(err, built)
			}
			if len(t) > 0 && !strings.HasPrefix(s, "COMMENT") {
				tag = t[len(t)-1].CommandTag.String()
			}
		}
		data = PGAdminDone{SQL: built.text(), Tag: tag}
	}
	if err != nil {
		return pgAdminFailed(err, built)
	}
	b, err := json.Marshal(data)
	if err != nil {
		return PGAdminResult{Error: err.Error()}
	}
	return PGAdminResult{OK: true, SQL: built.text(), Data: b}
}

func pgAdminFailed(err error, built adminSQL) PGAdminResult {
	r := PGAdminResult{Error: err.Error(), SQL: built.text()}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		r.Error, r.Code, r.Detail, r.Hint = pgErr.Message, pgErr.Code, pgErr.Detail, pgErr.Hint
	}
	return r
}

func listExtensions(ctx context.Context, conn *pgconn.PgConn) ([]PGExtension, error) {
	const sql = `SELECT ae.name, coalesce(e.extversion, ''), coalesce(ae.default_version, ''),
  coalesce(n.nspname, ''), coalesce(e.extrelocatable, false),
  coalesce((SELECT bool_or(v.trusted) FROM pg_available_extension_versions v WHERE v.name = ae.name AND v.version = ae.default_version), false),
  coalesce((SELECT bool_and(v.superuser) FROM pg_available_extension_versions v WHERE v.name = ae.name AND v.version = ae.default_version), true),
  coalesce(ae.comment, ''),
  coalesce((SELECT string_agg(v.version, E'\n' ORDER BY string_to_array(v.version, '.')::text[]) FROM pg_available_extension_versions v WHERE v.name = ae.name), '')
FROM pg_available_extensions ae
LEFT JOIN pg_extension e ON e.extname = ae.name
LEFT JOIN pg_namespace n ON n.oid = e.extnamespace
ORDER BY e.extname IS NULL, ae.name`
	res := conn.ExecParams(ctx, sql, nil, nil, nil, nil).Read()
	if res.Err != nil {
		return nil, res.Err
	}
	out := make([]PGExtension, 0, len(res.Rows))
	for _, r := range res.Rows {
		x := PGExtension{Name: string(r[0]), InstalledVersion: string(r[1]), DefaultVersion: string(r[2]),
			Schema: string(r[3]), Relocatable: string(r[4]) == "t", Trusted: string(r[5]) == "t",
			Superuser: string(r[6]) == "t", Comment: string(r[7])}
		if v := string(r[8]); v != "" {
			x.Versions = strings.Split(v, "\n")
		}
		out = append(out, x)
	}
	return out, nil
}

func setUpBy(comment string) string {
	if strings.HasPrefix(comment, adminMarker) {
		return strings.TrimPrefix(comment, adminMarker)
	}
	return ""
}

func listPublications(ctx context.Context, conn *pgconn.PgConn) ([]PGPublication, error) {
	const sql = `SELECT p.pubname, pg_get_userbyid(p.pubowner), p.puballtables, p.pubinsert, p.pubupdate,
  p.pubdelete, p.pubtruncate, p.pubviaroot, coalesce(obj_description(p.oid, 'pg_publication'), ''),
  coalesce((SELECT string_agg(quote_ident(t.schemaname) || '.' || quote_ident(t.tablename), E'\n' ORDER BY t.schemaname, t.tablename)
            FROM pg_publication_tables t WHERE t.pubname = p.pubname), '')
FROM pg_publication p ORDER BY p.pubname`
	res := conn.ExecParams(ctx, sql, nil, nil, nil, nil).Read()
	if res.Err != nil {
		return nil, res.Err
	}
	out := make([]PGPublication, 0, len(res.Rows))
	for _, r := range res.Rows {
		p := PGPublication{Name: string(r[0]), Owner: string(r[1]), AllTables: string(r[2]) == "t",
			ViaRoot: string(r[7]) == "t", SetUpBy: setUpBy(string(r[8])), Publish: []string{}}
		for i, o := range []string{"insert", "update", "delete", "truncate"} {
			if string(r[3+i]) == "t" {
				p.Publish = append(p.Publish, o)
			}
		}
		if t := string(r[9]); t != "" {
			p.Tables = strings.Split(t, "\n")
		}
		out = append(out, p)
	}
	return out, nil
}

// onOff turns a boolean column's text into on/off, and leaves "" alone.
func onOff(b string) string {
	switch b {
	case "t", "true":
		return "on"
	case "f", "false":
		return "off"
	}
	return b
}

func listSubscriptions(ctx context.Context, conn *pgconn.PgConn) ([]PGSubscription, error) {
	major := pgMajor(conn.ParameterStatus("server_version"))
	errs := "NULL::bigint, NULL::bigint"
	join := ""
	if major >= 15 {
		errs = "ss.apply_error_count, ss.sync_error_count"
		join = "LEFT JOIN pg_stat_subscription_stats ss ON ss.subid = s.oid"
	}
	// How it applies: the columns arrived in 14 (binary, streaming) and 15
	// (disable_on_error); streaming is a bool in 14 and a char in 16.
	opts := "'', '', ''"
	if major >= 16 {
		opts = "s.subbinary::text, CASE s.substream WHEN 'f' THEN 'off' WHEN 't' THEN 'on' WHEN 'p' THEN 'parallel' ELSE s.substream::text END, s.subdisableonerr::text"
	} else if major == 15 {
		opts = "s.subbinary::text, CASE WHEN s.substream THEN 'on' ELSE 'off' END, s.subdisableonerr::text"
	} else if major == 14 {
		opts = "s.subbinary::text, CASE WHEN s.substream THEN 'on' ELSE 'off' END, ''"
	}
	sql := `SELECT s.subname, s.subenabled, s.subconninfo, array_to_string(s.subpublications, E'\n'),
  coalesce(s.subslotname, ''), pg_get_userbyid(s.subowner), coalesce(obj_description(s.oid, 'pg_subscription'), ''),
  coalesce(st.pid::text, ''), coalesce(st.received_lsn::text, ''), coalesce(st.latest_end_lsn::text, ''),
  coalesce(to_char(st.last_msg_receipt_time AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), ''),
  coalesce(to_char(st.latest_end_time AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), ''),
  coalesce(floor(extract(epoch FROM now() - st.latest_end_time))::bigint::text, ''),
  (SELECT count(*) FROM pg_subscription_rel r WHERE r.srsubid = s.oid AND r.srsubstate = 'r'),
  (SELECT count(*) FROM pg_subscription_rel r WHERE r.srsubid = s.oid AND r.srsubstate IN ('d', 's', 'f')),
  (SELECT count(*) FROM pg_subscription_rel r WHERE r.srsubid = s.oid AND r.srsubstate = 'i'),
  ` + errs + `, coalesce(s.subsynccommit, ''), ` + opts + `
FROM pg_subscription s
LEFT JOIN pg_stat_subscription st ON st.subid = s.oid AND st.relid IS NULL
` + join + `
WHERE s.subdbid = (SELECT oid FROM pg_database WHERE datname = current_database())
ORDER BY s.subname`
	res := conn.ExecParams(ctx, sql, nil, nil, nil, nil).Read()
	if res.Err != nil {
		return nil, res.Err
	}
	out := make([]PGSubscription, 0, len(res.Rows))
	num := func(b []byte) *int64 {
		if len(b) == 0 {
			return nil
		}
		n, err := strconv.ParseInt(string(b), 10, 64)
		if err != nil {
			return nil
		}
		return &n
	}
	for _, r := range res.Rows {
		ci := string(r[2])
		s := PGSubscription{Name: string(r[0]), Enabled: string(r[1]) == "t",
			Publisher: publisherOf(ci), Conninfo: redactConninfo(ci), Publications: []string{},
			Slot: string(r[4]), Owner: string(r[5]), SetUpBy: setUpBy(string(r[6])),
			ReceivedLSN: string(r[8]), LatestEndLSN: string(r[9]), LastMessageAt: string(r[10]),
			LatestEndAt: string(r[11]), ApplyLagS: num(r[12]), SyncCommit: string(r[18]),
			Binary: onOff(string(r[19])), Streaming: string(r[20]), DisableOnError: onOff(string(r[21]))}
		if p := string(r[3]); p != "" {
			s.Publications = strings.Split(p, "\n")
		}
		s.WorkerPID, _ = strconv.Atoi(string(r[7]))
		s.TablesReady, _ = strconv.Atoi(string(r[13]))
		s.TablesSyncing, _ = strconv.Atoi(string(r[14]))
		s.TablesWaiting, _ = strconv.Atoi(string(r[15]))
		s.ApplyErrors, s.SyncErrors = num(r[16]), num(r[17])
		out = append(out, s)
	}
	return out, nil
}
