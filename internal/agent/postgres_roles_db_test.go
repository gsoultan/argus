package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

// Roles and privileges, end to end through a DBA session against a real
// Postgres, starting from the case that prompted them: an application role
// that needs the tables of a schema it does not own.
func TestPostgresRolesAndGrants(t *testing.T) {
	dsn, _, _, dbname := pgTest(t)
	const password = "hermod-correct-horse-battery"
	superTry(t, dsn, "DROP SCHEMA IF EXISTS etl CASCADE", "DROP OWNED BY argus_t_hermod", "DROP ROLE IF EXISTS argus_t_hermod",
		"DROP ROLE IF EXISTS argus_t_hermod2", "DROP OWNED BY argus_t_etl_owner", "DROP ROLE IF EXISTS argus_t_etl_owner",
		"DROP ROLE IF EXISTS argus_t_readers", "DROP ROLE IF EXISTS argus_t_person")
	superExec(t, dsn,
		// An owner Argus never hands its owner level: one that can create
		// roles, like an application's migration account often can.
		"CREATE ROLE argus_t_etl_owner NOLOGIN CREATEROLE",
		"CREATE ROLE argus_t_readers NOLOGIN",
		"CREATE ROLE argus_t_person NOLOGIN",
		"COMMENT ON ROLE argus_t_person IS 'argus-managed owner=someone@corp.example'",
		"CREATE SCHEMA etl AUTHORIZATION argus_t_etl_owner",
		"SET ROLE argus_t_etl_owner",
		"CREATE TABLE etl.jobs (id int)",
		"CREATE TABLE etl.runs (id int)",
		"RESET ROLE",
	)
	h := openPG(t, tunnel.LevelDBA, nil)
	id := uint32(100)
	ok := func(op tunnel.PGAdminOp) tunnel.PGAnswer {
		t.Helper()
		id++
		a := h.admin(id, op)
		if a.Type != "admin" {
			t.Fatalf("%s: %+v", op.Kind, a)
		}
		return a
	}
	refused := func(op tunnel.PGAdminOp, why string) {
		t.Helper()
		id++
		if a := h.admin(id, op); a.Type != "error" || !strings.Contains(a.Error, why) {
			t.Fatalf("%s should be refused with %q: %+v", op.Kind, why, a)
		}
	}
	yes := true

	// Create the application's role with a password.
	ok(tunnel.PGAdminOp{Kind: tunnel.AdminRoleCreate, RoleSpec: &tunnel.PGRoleSpec{Name: "argus_t_hermod", Login: &yes,
		Password: password, MemberOf: []string{"argus_t_readers"}}})
	got := superQuery(t, dsn, "SELECT rolcanlogin::text, rolpassword FROM pg_authid WHERE rolname = 'argus_t_hermod'")
	if got[0][0] != "true" || !strings.HasPrefix(got[0][1], "SCRAM-SHA-256$") {
		t.Fatalf("hermod: %v", got)
	}
	if superQuery(t, dsn, "SELECT count(*) FROM pg_auth_members m JOIN pg_roles g ON g.oid = m.roleid JOIN pg_roles r ON r.oid = m.member WHERE g.rolname = 'argus_t_readers' AND r.rolname = 'argus_t_hermod'")[0][0] != "1" {
		t.Fatal("hermod did not join readers")
	}

	// The bug this exists for: the same GRANT typed into the editor runs as
	// the DBA level role, which does not own etl's tables, and Postgres says
	// only "no privileges were granted".
	superExec(t, dsn, "GRANT USAGE ON SCHEMA etl TO "+h.ready.Role)
	h.run(150, "GRANT SELECT ON ALL TABLES IN SCHEMA etl TO argus_t_hermod", 0)
	if superQuery(t, dsn, "SELECT has_table_privilege('argus_t_hermod', 'etl.jobs', 'SELECT')::text")[0][0] != "false" {
		t.Fatal("the level role's GRANT took; this test's premise is gone")
	}

	// Through the helper, it takes, and tables made later are covered too.
	ok(tunnel.PGAdminOp{Kind: tunnel.AdminGrant, GrantSpec: &tunnel.PGGrantSpec{Grantee: "argus_t_hermod", On: tunnel.OnAllTables,
		Schema: "etl", Privileges: []string{"SELECT", "INSERT", "UPDATE", "DELETE"}, SchemaUsage: true,
		Future: true, ForRoles: []string{"argus_t_etl_owner"}}})
	superExec(t, dsn, "SET ROLE argus_t_etl_owner", "CREATE TABLE etl.later (id int)", "RESET ROLE")
	for _, tb := range []string{"etl.jobs", "etl.runs", "etl.later"} {
		if superQuery(t, dsn, "SELECT has_table_privilege('argus_t_hermod', '"+tb+"', 'INSERT')::text")[0][0] != "true" {
			t.Fatalf("hermod cannot insert into %s", tb)
		}
	}
	if superQuery(t, dsn, "SELECT has_schema_privilege('argus_t_hermod', 'etl', 'USAGE')::text")[0][0] != "true" {
		t.Fatal("hermod has no usage on etl")
	}

	// What hermod can do here, as the console shows it.
	a := ok(tunnel.PGAdminOp{Kind: tunnel.AdminRolePrivileges, RoleSpec: &tunnel.PGRoleSpec{Name: "argus_t_hermod"}})
	var privs PGRolePrivileges
	if err := json.Unmarshal(a.Admin, &privs); err != nil {
		t.Fatal(err)
	}
	var jobs *PGPrivRow
	for i := range privs.Tables {
		if privs.Tables[i].Schema == "etl" && privs.Tables[i].Name == "jobs" {
			jobs = &privs.Tables[i]
		}
	}
	if jobs == nil || strings.Join(jobs.Effective, ",") != "SELECT,INSERT,UPDATE,DELETE" || len(jobs.Direct) != 4 || jobs.Owner != "argus_t_etl_owner" {
		t.Fatalf("etl.jobs for hermod: %+v", jobs)
	}
	if len(privs.Defaults) != 1 || privs.Defaults[0].ForRole != "argus_t_etl_owner" || privs.Defaults[0].Objects != "tables" {
		t.Fatalf("default privileges: %+v", privs.Defaults)
	}
	if privs.Database.Name != dbname {
		t.Fatalf("database row: %+v", privs.Database)
	}

	// The objects a grant can name, with whose new tables to cover.
	a = ok(tunnel.PGAdminOp{Kind: tunnel.AdminGrantObjects, GrantSpec: &tunnel.PGGrantSpec{Schema: "etl"}})
	var objs PGGrantObjects
	_ = json.Unmarshal(a.Admin, &objs)
	if len(objs.Tables) != 3 || len(objs.Owners) != 1 || objs.Owners[0] != "argus_t_etl_owner" {
		t.Fatalf("grant.objects: %+v", objs)
	}

	// Revoke takes it back, the defaults too.
	ok(tunnel.PGAdminOp{Kind: tunnel.AdminRevoke, GrantSpec: &tunnel.PGGrantSpec{Grantee: "argus_t_hermod", On: tunnel.OnAllTables,
		Schema: "etl", Privileges: []string{"INSERT", "UPDATE", "DELETE"}, Future: true, ForRoles: []string{"argus_t_etl_owner"}}})
	if superQuery(t, dsn, "SELECT has_table_privilege('argus_t_hermod', 'etl.later', 'INSERT')::text || has_table_privilege('argus_t_hermod', 'etl.later', 'SELECT')::text")[0][0] != "falsetrue" {
		t.Fatal("the revoke did not take only what it named")
	}

	// A change that fails part-way changes nothing: the grant on a table
	// that does not exist rolls back the usage granted before it.
	superExec(t, dsn, "CREATE SCHEMA argus_t_empty AUTHORIZATION argus_t_etl_owner")
	t.Cleanup(func() { superTry(t, dsn, "DROP SCHEMA IF EXISTS argus_t_empty CASCADE") })
	refused(tunnel.PGAdminOp{Kind: tunnel.AdminGrant, GrantSpec: &tunnel.PGGrantSpec{Grantee: "argus_t_hermod", On: tunnel.OnTable,
		Schema: "argus_t_empty", Objects: []string{"nope"}, Privileges: []string{"SELECT"}, SchemaUsage: true}}, "does not exist")
	if superQuery(t, dsn, "SELECT has_schema_privilege('argus_t_hermod', 'argus_t_empty', 'USAGE')::text")[0][0] != "false" {
		t.Fatal("half a failed change stayed")
	}

	// Change and rename; the password changes too.
	before := superQuery(t, dsn, "SELECT rolpassword FROM pg_authid WHERE rolname = 'argus_t_hermod'")[0][0]
	limit := 3
	ok(tunnel.PGAdminOp{Kind: tunnel.AdminRoleAlter, RoleSpec: &tunnel.PGRoleSpec{Name: "argus_t_hermod", Rename: "argus_t_hermod2",
		ConnLimit: &limit, Password: "another-" + password}})
	got = superQuery(t, dsn, "SELECT rolconnlimit::text, rolpassword FROM pg_authid WHERE rolname = 'argus_t_hermod2'")
	if len(got) != 1 || got[0][0] != "3" || got[0][1] == before {
		t.Fatalf("after alter: %v", got)
	}

	// Argus's own roles and the helper's account are not this panel's.
	refused(tunnel.PGAdminOp{Kind: tunnel.AdminRoleDrop, RoleSpec: &tunnel.PGRoleSpec{Name: "argus_t_person"}}, "person's role")
	refused(tunnel.PGAdminOp{Kind: tunnel.AdminGrant, GrantSpec: &tunnel.PGGrantSpec{Grantee: "argus_t_person", On: tunnel.OnSchema,
		Schema: "etl", Privileges: []string{"USAGE"}}}, "person's role")
	refused(tunnel.PGAdminOp{Kind: tunnel.AdminRoleMember, Member: &tunnel.PGMemberSpec{Role: "argus_t_readers", Member: h.ready.Role}}, "Argus")
	no := false
	helper := superQuery(t, dsn, "SELECT current_user")[0][0]
	refused(tunnel.PGAdminOp{Kind: tunnel.AdminRoleAlter, RoleSpec: &tunnel.PGRoleSpec{Name: helper, Superuser: &no}}, "keeps SUPERUSER")
	refused(tunnel.PGAdminOp{Kind: tunnel.AdminRoleCreate, RoleSpec: &tunnel.PGRoleSpec{Name: "argus_t_readers"}}, "already exists")
	// A level role may still be given to an application role.
	if lvl := h.ready.Levels[tunnel.LevelRead]; lvl != "" {
		ok(tunnel.PGAdminOp{Kind: tunnel.AdminRoleMember, Member: &tunnel.PGMemberSpec{Role: lvl, Member: "argus_t_hermod2"}})
		ok(tunnel.PGAdminOp{Kind: tunnel.AdminRoleUnmember, Member: &tunnel.PGMemberSpec{Role: lvl, Member: "argus_t_hermod2"}})
	}

	// Listed with what the console needs.
	a = ok(tunnel.PGAdminOp{Kind: tunnel.AdminRoleList})
	var roles []PGRole
	_ = json.Unmarshal(a.Admin, &roles)
	var hermod *PGRole
	sawHelper := false
	for i := range roles {
		if roles[i].Name == "argus_t_hermod2" {
			hermod = &roles[i]
		}
		sawHelper = sawHelper || roles[i].Helper
	}
	if hermod == nil || !hermod.Login || !hermod.HasPassword || hermod.ConnLimit != 3 || len(hermod.MemberOf) != 1 || !sawHelper {
		t.Fatalf("role.list: %+v (helper seen %v)", hermod, sawHelper)
	}

	// Drop, handing what it owns to someone else first.
	superExec(t, dsn, "GRANT CREATE ON SCHEMA etl TO argus_t_hermod2", "SET ROLE argus_t_hermod2", "CREATE TABLE etl.mine (id int)", "RESET ROLE")
	ok(tunnel.PGAdminOp{Kind: tunnel.AdminRoleDrop, RoleSpec: &tunnel.PGRoleSpec{Name: "argus_t_hermod2", ReassignTo: "argus_t_etl_owner", DropOwned: true}})
	if superQuery(t, dsn, "SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid = 'etl.mine'::regclass")[0][0] != "argus_t_etl_owner" {
		t.Fatal("its table was not handed over")
	}

	// Recorded before running, password nowhere: not in the recording, not
	// in any answer's text.
	h.close()
	var adminLines int
	for _, rec := range h.rec {
		if rec.Kind == "admin" {
			adminLines++
			if strings.Contains(rec.SQL, password) || strings.Contains(rec.SQL, "SCRAM-SHA-256") {
				t.Fatalf("a password reached the recording: %s", rec.SQL)
			}
		}
	}
	if adminLines < 8 {
		t.Fatalf("only %d admin lines were recorded", adminLines)
	}
	files, _ := filepath.Glob(filepath.Join(h.dir, "*"))
	for _, f := range files {
		b, _ := os.ReadFile(f)
		if strings.Contains(string(b), password) {
			t.Fatalf("%s holds the password", f)
		}
	}
	superTry(t, dsn, "DROP SCHEMA IF EXISTS etl CASCADE", "DROP ROLE IF EXISTS argus_t_readers", "DROP ROLE IF EXISTS argus_t_person",
		"DROP OWNED BY argus_t_etl_owner", "DROP ROLE IF EXISTS argus_t_etl_owner")
}
