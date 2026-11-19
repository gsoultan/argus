//go:build !windows

package agent

import (
	"database/sql"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

func msAdminData[T any](t *testing.T, a tunnel.PGAnswer) T {
	t.Helper()
	var v T
	if a.Type != "admin" {
		t.Fatalf("admin answer: %+v", a)
	}
	if err := json.Unmarshal(a.Admin, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// Users, roles, membership and GRANT, REVOKE and DENY through a DBA session:
// refused below dba, run as the admin login, recorded before they run with
// the password redacted, and what a user can do read back from SQL Server as
// that user.
func TestMSSQLAccess(t *testing.T) {
	m := newMSService(t)
	pw, port := msTest(t)
	root := msRoot(t, pw, port)
	msDB(t, root, "argus_t_access")
	msDB(t, root, "argus_t_access2")
	const secret = "Hunter2-the-app-password!"
	dropLogins := func() {
		for _, l := range []string{"app_reader", "shared_login", "boss_login"} {
			_, _ = root.Exec("IF SUSER_ID(N'" + l + "') IS NOT NULL DROP LOGIN [" + l + "]")
		}
	}
	dropLogins()
	t.Cleanup(dropLogins)
	msExec(t, root,
		"CREATE LOGIN shared_login WITH PASSWORD = 'Shared!Pass123', CHECK_POLICY = OFF",
		"CREATE LOGIN boss_login WITH PASSWORD = 'Boss!Pass123', CHECK_POLICY = OFF",
		"ALTER SERVER ROLE sysadmin ADD MEMBER boss_login",
		"USE argus_t_access2; CREATE USER shared_login FOR LOGIN shared_login",
		"USE argus_t_access; CREATE TABLE dbo.orders (id INT PRIMARY KEY); CREATE TABLE dbo.secrets (id INT)",
		"USE argus_t_access; EXEC('CREATE SCHEMA sales'); CREATE TABLE sales.leads (id INT)",
		"USE master")

	// Below dba: no.
	w := m.mustOpen(t, "argus_t_access", tunnel.LevelWrite)
	if a := w.admin(1, tunnel.PGAdminOp{Kind: tunnel.AdminMSPrincipals}); a.Type != "error" || a.Code != "42501" {
		t.Fatalf("write listed principals: %+v", a)
	}
	w.close()

	h := m.mustOpen(t, "argus_t_access", tunnel.LevelDBA)
	ok := func(id uint32, op tunnel.PGAdminOp) tunnel.PGAnswer {
		t.Helper()
		a := h.admin(id, op)
		if a.Type != "admin" {
			t.Fatalf("%s: %+v", op.Kind, a)
		}
		return a
	}
	refused := func(id uint32, op tunnel.PGAdminOp, want string) {
		t.Helper()
		if a := h.admin(id, op); a.Type != "error" || !strings.Contains(a.Error, want) {
			t.Fatalf("%s: %+v, want an error with %q", op.Kind, a, want)
		}
	}
	str := func(s string) *string { return &s }

	// A preview says what would run, with the password redacted, and runs
	// nothing.
	pv := msAdminData[PGAdminDone](t, ok(2, tunnel.PGAdminOp{Kind: tunnel.AdminMSUserCreate, Preview: true,
		Principal: &tunnel.MSSQLPrincipalSpec{Name: "app_reader", Auth: tunnel.MSAuthSQL, Password: secret,
			DefaultSchema: str("sales"), MemberOf: []string{"db_datareader"}}}))
	if !strings.Contains(pv.SQL, "PASSWORD = [redacted]") || strings.Contains(pv.SQL, secret) ||
		!strings.Contains(pv.SQL, "ALTER ROLE [db_datareader] ADD MEMBER [app_reader]") {
		t.Fatalf("preview: %s", pv.SQL)
	}
	var n int
	if root.QueryRow("SELECT COUNT(*) FROM sys.server_principals WHERE name = 'app_reader'").Scan(&n); n != 0 {
		t.Fatal("a preview made the login")
	}

	// A user with a new SQL login: it signs in with the password, in this
	// database, with its default schema and role.
	ok(3, tunnel.PGAdminOp{Kind: tunnel.AdminMSUserCreate,
		Principal: &tunnel.MSSQLPrincipalSpec{Name: "app_reader", Auth: tunnel.MSAuthSQL, Password: secret,
			DefaultSchema: str("sales"), MemberOf: []string{"db_datareader"}}})
	app, err := sql.Open("sqlserver", mssqlDSN("127.0.0.1", port, "app_reader", secret, "argus_t_access"))
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	var schema string
	if err := app.QueryRow("SELECT SCHEMA_NAME()").Scan(&schema); err != nil || schema != "sales" {
		t.Fatalf("app_reader signs in: %v %q", err, schema)
	}
	if _, err := app.Exec("INSERT INTO dbo.orders VALUES (1)"); err == nil {
		t.Fatal("a reader inserted")
	}

	// A role, granted on a schema, given to the user; a deny wins over it.
	ok(4, tunnel.PGAdminOp{Kind: tunnel.AdminMSRoleCreate, Principal: &tunnel.MSSQLPrincipalSpec{Name: "sales_writers"}})
	ok(5, tunnel.PGAdminOp{Kind: tunnel.AdminMSGrant, MSGrant: &tunnel.MSSQLGrantSpec{Grantee: "sales_writers",
		On: tunnel.MSOnSchema, Schema: "sales", Privileges: []string{"INSERT", "UPDATE"}}})
	ok(6, tunnel.PGAdminOp{Kind: tunnel.AdminMSMember, Member: &tunnel.PGMemberSpec{Role: "sales_writers", Member: "app_reader"}})
	ok(7, tunnel.PGAdminOp{Kind: tunnel.AdminMSDeny, MSGrant: &tunnel.MSSQLGrantSpec{Grantee: "app_reader",
		On: tunnel.MSOnObject, Schema: "dbo", Objects: []string{"secrets"}, Privileges: []string{"SELECT"}}})
	if _, err := app.Exec("INSERT INTO sales.leads VALUES (1)"); err != nil {
		t.Fatalf("through the role: %v", err)
	}
	if _, err := app.Exec("SELECT * FROM dbo.secrets"); err == nil {
		t.Fatal("the deny did not hold")
	}

	// What it can do, as SQL Server says it.
	perms := msAdminData[MSPermissions](t, ok(8, tunnel.PGAdminOp{Kind: tunnel.AdminMSPermissions,
		Principal: &tunnel.MSSQLPrincipalSpec{Name: "app_reader"}}))
	roles := []string{}
	for _, r := range perms.MemberOf {
		roles = append(roles, r.Role)
	}
	if !slices.Contains(roles, "db_datareader") || !slices.Contains(roles, "sales_writers") {
		t.Fatalf("member of: %+v", perms.MemberOf)
	}
	if len(perms.Direct) == 0 || !slices.ContainsFunc(perms.Direct, func(p MSPermission) bool {
		return p.State == "DENY" && p.Object == "secrets" && p.Permission == "SELECT"
	}) {
		t.Fatalf("direct: %+v", perms.Direct)
	}
	if !slices.ContainsFunc(perms.Inherited, func(p MSPermission) bool {
		return p.Via == "sales_writers" && p.On == "schema" && p.Schema == "sales" && p.Permission == "INSERT"
	}) {
		t.Fatalf("inherited: %+v", perms.Inherited)
	}
	can := map[string][]string{}
	for _, o := range perms.Objects {
		can[o.Schema+"."+o.Name] = o.Can
	}
	if !slices.Contains(can["sales.leads"], "INSERT") || slices.Contains(can["dbo.secrets"], "SELECT") ||
		!slices.Contains(can["dbo.orders"], "SELECT") || slices.Contains(can["dbo.orders"], "INSERT") {
		t.Fatalf("effective: %+v (%s)", can, perms.Unknown)
	}
	if !slices.Contains(perms.Database, "CONNECT") {
		t.Fatalf("database: %v", perms.Database)
	}

	// The lists.
	list := msAdminData[MSPrincipals](t, ok(9, tunnel.PGAdminOp{Kind: tunnel.AdminMSPrincipals}))
	byName := map[string]MSPrincipal{}
	for _, p := range list.Principals {
		byName[p.Name] = p
	}
	if u := byName["app_reader"]; u.Type != "user" || u.Auth != "sql" || u.Login != "app_reader" || u.DefaultSchema != "sales" {
		t.Fatalf("app_reader: %+v", u)
	}
	if r := byName["sales_writers"]; r.Type != "role" || r.Members != 1 {
		t.Fatalf("sales_writers: %+v", r)
	}
	if a := byName[tunnel.MSSQLLogin("ana_sari", "dba", "argus_t_access")]; !a.Argus {
		t.Fatalf("the session's own user is not marked Argus's: %+v", list.Principals)
	}
	if !slices.Contains(list.Logins, "shared_login") || slices.Contains(list.Logins, "app_reader") {
		t.Fatalf("logins: %v", list.Logins)
	}
	objs := msAdminData[MSObjects](t, ok(10, tunnel.PGAdminOp{Kind: tunnel.AdminMSObjects, MSGrant: &tunnel.MSSQLGrantSpec{Schema: "sales"}}))
	if len(objs.Objects) != 1 || objs.Objects[0].Name != "leads" || !slices.ContainsFunc(objs.Schemas, func(o MSObject) bool { return o.Name == "sales" }) {
		t.Fatalf("objects: %+v", objs)
	}

	// Changing the user: its password through its own login, disabling it,
	// and a rename.
	ok(11, tunnel.PGAdminOp{Kind: tunnel.AdminMSUserAlter, Principal: &tunnel.MSSQLPrincipalSpec{Name: "app_reader",
		Login: "app_reader", Password: "Second!Pass456", Enabled: new(bool)}})
	var disabled bool
	if err := root.QueryRow("SELECT is_disabled FROM sys.sql_logins WHERE name = 'app_reader'").Scan(&disabled); err != nil || !disabled {
		t.Fatalf("disabled: %v %v", err, disabled)
	}
	var matches int
	if err := root.QueryRow("SELECT PWDCOMPARE(N'Second!Pass456', password_hash) FROM sys.sql_logins WHERE name = 'app_reader'").Scan(&matches); err != nil || matches != 1 {
		t.Fatalf("the new password: %v %d", err, matches)
	}

	// Logins that are not this user's, or hold server rights: refused.
	ok(12, tunnel.PGAdminOp{Kind: tunnel.AdminMSUserCreate, Principal: &tunnel.MSSQLPrincipalSpec{Name: "boss", Auth: tunnel.MSAuthLogin, Login: "boss_login"}})
	refused(13, tunnel.PGAdminOp{Kind: tunnel.AdminMSUserAlter, Principal: &tunnel.MSSQLPrincipalSpec{Name: "app_reader",
		Login: "shared_login", Password: "x"}}, "is not the one")
	refused(14, tunnel.PGAdminOp{Kind: tunnel.AdminMSUserAlter, Principal: &tunnel.MSSQLPrincipalSpec{Name: "boss",
		Login: "boss_login", Password: "Taken!Over1"}}, "server-wide rights")
	ok(15, tunnel.PGAdminOp{Kind: tunnel.AdminMSUserCreate, Principal: &tunnel.MSSQLPrincipalSpec{Name: "shared", Auth: tunnel.MSAuthLogin, Login: "shared_login"}})
	refused(16, tunnel.PGAdminOp{Kind: tunnel.AdminMSUserDrop, Principal: &tunnel.MSSQLPrincipalSpec{Name: "shared",
		Login: "shared_login", DropLogin: true}}, "also has a user in argus_t_access2")
	// Argus's own, and SQL Server's: refused before anything runs.
	refused(17, tunnel.PGAdminOp{Kind: tunnel.AdminMSUserDrop, Principal: &tunnel.MSSQLPrincipalSpec{
		Name: tunnel.MSSQLLogin("ana_sari", "dba", "argus_t_access")}}, "Argus's own")
	refused(18, tunnel.PGAdminOp{Kind: tunnel.AdminMSRoleDrop, Principal: &tunnel.MSSQLPrincipalSpec{Name: "db_owner"}}, "fixed roles")

	// One that fails rolls back whole: the role is made and dropped again.
	if a := h.admin(19, tunnel.PGAdminOp{Kind: tunnel.AdminMSRoleCreate, Principal: &tunnel.MSSQLPrincipalSpec{
		Name: "half_made", MemberOf: []string{"no_such_role"}}}); a.Type != "error" {
		t.Fatalf("membership in a missing role: %+v", a)
	}
	if err := root.QueryRow("SELECT COUNT(*) FROM argus_t_access.sys.database_principals WHERE name = 'half_made'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("half_made left behind: %v %d", err, n)
	}

	// Revoke and drop: the user and its login go.
	ok(20, tunnel.PGAdminOp{Kind: tunnel.AdminMSRevoke, MSGrant: &tunnel.MSSQLGrantSpec{Grantee: "app_reader",
		On: tunnel.MSOnObject, Schema: "dbo", Objects: []string{"secrets"}, Privileges: []string{"SELECT"}}})
	ok(21, tunnel.PGAdminOp{Kind: tunnel.AdminMSUnmember, Member: &tunnel.PGMemberSpec{Role: "sales_writers", Member: "app_reader"}})
	ok(22, tunnel.PGAdminOp{Kind: tunnel.AdminMSUserDrop, Principal: &tunnel.MSSQLPrincipalSpec{Name: "app_reader",
		Login: "app_reader", DropLogin: true}})
	if err := root.QueryRow("SELECT COUNT(*) FROM sys.server_principals WHERE name = 'app_reader'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("the login was left: %v %d", err, n)
	}
	h.close()

	// The recording has every change before its outcome, and no password.
	b, err := os.ReadFile(m.dir + "/" + msRecording(t, m.dir, "dba"))
	if err != nil {
		t.Fatal(err)
	}
	rec := string(b)
	for _, s := range []string{secret, "Second!Pass456", "0x0200"} {
		if strings.Contains(rec, s) {
			t.Fatalf("the recording holds %q", s)
		}
	}
	for _, s := range []string{`CREATE LOGIN [app_reader] WITH PASSWORD = [redacted]`, `DENY SELECT ON OBJECT::[dbo].[secrets] TO [app_reader]`,
		`DROP LOGIN [app_reader]`, `"kind":"admin"`} {
		if !strings.Contains(rec, s) {
			t.Fatalf("the recording lacks %q:\n%s", s, rec)
		}
	}
}

// msRecording is the one recording of a session whose first line names the
// level's login.
func msRecording(t *testing.T, dir, level string) string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		b, _ := os.ReadFile(dir + "/" + e.Name())
		if strings.Contains(strings.SplitN(string(b), "\n", 2)[0], "-"+level+"-") {
			return e.Name()
		}
	}
	t.Fatalf("no recording at %s", level)
	return ""
}

func TestMSSQLPasswordHash(t *testing.T) {
	pw, port := msTest(t)
	root := msRoot(t, pw, port)
	h, err := tunnel.MSSQLPasswordHash("Pässwörd!1")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = root.Exec("IF SUSER_ID(N'argus_t_hash') IS NOT NULL DROP LOGIN argus_t_hash")
	msExec(t, root, "CREATE LOGIN argus_t_hash WITH PASSWORD = "+h+" HASHED, CHECK_POLICY = OFF")
	defer root.Exec("DROP LOGIN argus_t_hash")
	var ok int
	if err := root.QueryRow("SELECT PWDCOMPARE(N'Pässwörd!1', password_hash) FROM sys.sql_logins WHERE name = 'argus_t_hash'").Scan(&ok); err != nil || ok != 1 {
		t.Fatalf("hash: %v %d", err, ok)
	}
}
