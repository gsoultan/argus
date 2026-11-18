package tunnel

import (
	"strings"
	"testing"
)

func yes() *bool { b := true; return &b }
func no() *bool  { b := false; return &b }

func TestPGRoleSQL(t *testing.T) {
	limit, until, comment := 5, "2027-01-31", "ETL loader"
	cases := []struct {
		name string
		op   PGAdminOp
		want string
	}{
		{"create", PGAdminOp{Kind: AdminRoleCreate, RoleSpec: &PGRoleSpec{Name: "hermod", Login: yes(), ConnLimit: &limit,
			ValidUntil: &until, Password: "s3cret'pw", Comment: &comment, MemberOf: []string{"readers"}}},
			`CREATE ROLE "hermod" WITH LOGIN CONNECTION LIMIT 5 VALID UNTIL E'2027-01-31' PASSWORD [redacted];
COMMENT ON ROLE "hermod" IS E'ETL loader';
GRANT "readers" TO "hermod"`},
		{"create bare", PGAdminOp{Kind: AdminRoleCreate, RoleSpec: &PGRoleSpec{Name: "app"}}, `CREATE ROLE "app"`},
		{"alter and rename", PGAdminOp{Kind: AdminRoleAlter, RoleSpec: &PGRoleSpec{Name: "app", Rename: "app2", CreateDB: no(), NoPassword: true}},
			`ALTER ROLE "app" WITH NOCREATEDB PASSWORD NULL;
ALTER ROLE "app" RENAME TO "app2"`},
		{"drop", PGAdminOp{Kind: AdminRoleDrop, RoleSpec: &PGRoleSpec{Name: "old", ReassignTo: "postgres", DropOwned: true}},
			`REASSIGN OWNED BY "old" TO "postgres";
DROP OWNED BY "old";
DROP ROLE "old"`},
		{"member", PGAdminOp{Kind: AdminRoleMember, Member: &PGMemberSpec{Role: "readers", Member: "hermod", Admin: true}},
			`GRANT "readers" TO "hermod" WITH ADMIN OPTION`},
		{"unmember", PGAdminOp{Kind: AdminRoleUnmember, Member: &PGMemberSpec{Role: "readers", Member: "hermod"}},
			`REVOKE "readers" FROM "hermod"`},
		{"hermod's grant", PGAdminOp{Kind: AdminGrant, GrantSpec: &PGGrantSpec{Grantee: "hermod", On: OnAllTables, Schema: "etl",
			Privileges: []string{"DELETE", "SELECT", "INSERT", "UPDATE"}, SchemaUsage: true, Future: true, ForRoles: []string{"etl_owner"}}},
			`GRANT USAGE ON SCHEMA "etl" TO "hermod";
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA "etl" TO "hermod";
ALTER DEFAULT PRIVILEGES FOR ROLE "etl_owner" IN SCHEMA "etl" GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO "hermod"`},
		{"database", PGAdminOp{Kind: AdminGrant, GrantSpec: &PGGrantSpec{Grantee: "hermod", On: OnDatabase, Privileges: []string{"CONNECT"}}},
			`GRANT CONNECT ON DATABASE "athena_etl_db" TO "hermod"`},
		{"tables", PGAdminOp{Kind: AdminGrant, GrantSpec: &PGGrantSpec{Grantee: "PUBLIC", On: OnTable, Schema: "etl", Objects: []string{"jobs", `we"ird`},
			Privileges: []string{"ALL"}, WithGrant: true}},
			`GRANT ALL PRIVILEGES ON TABLE "etl"."jobs", "etl"."we""ird" TO PUBLIC WITH GRANT OPTION`},
		{"function", PGAdminOp{Kind: AdminGrant, GrantSpec: &PGGrantSpec{Grantee: "hermod", On: OnFunction, Schema: "etl",
			Objects: []string{"load(integer, character varying)"}, Privileges: []string{"EXECUTE"}}},
			`GRANT EXECUTE ON ROUTINE "etl".load(integer, character varying) TO "hermod"`},
		{"revoke", PGAdminOp{Kind: AdminRevoke, GrantSpec: &PGGrantSpec{Grantee: "hermod", On: OnAllSequences, Schema: "etl",
			Privileges: []string{"USAGE"}, Cascade: true, Future: true, ForRoles: []string{"a", "b"}}},
			`REVOKE USAGE ON ALL SEQUENCES IN SCHEMA "etl" FROM "hermod" CASCADE;
ALTER DEFAULT PRIVILEGES FOR ROLE "a" IN SCHEMA "etl" REVOKE USAGE ON SEQUENCES FROM "hermod" CASCADE;
ALTER DEFAULT PRIVILEGES FOR ROLE "b" IN SCHEMA "etl" REVOKE USAGE ON SEQUENCES FROM "hermod" CASCADE`},
		{"grant option only", PGAdminOp{Kind: AdminRevoke, GrantSpec: &PGGrantSpec{Grantee: "hermod", On: OnSchema, Schema: "etl",
			Privileges: []string{"CREATE"}, WithGrant: true}},
			`REVOKE GRANT OPTION FOR CREATE ON SCHEMA "etl" FROM "hermod"`},
	}
	for _, c := range cases {
		got, err := PGRoleText(&c.op, "athena_etl_db")
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}

// The password is in what runs, as the verifier, and never in the text.
func TestPGRoleSQLPassword(t *testing.T) {
	op := PGAdminOp{Kind: AdminRoleAlter, RoleSpec: &PGRoleSpec{Name: "hermod", Password: "hunter2"}}
	run, text, err := PGRoleSQL(&op, "db", "SCRAM-SHA-256$4096:c2FsdA==$a2V5:c2Vydg==")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(run[0], "SCRAM-SHA-256$4096") || strings.Contains(run[0], "hunter2") {
		t.Fatalf("run: %s", run[0])
	}
	if strings.Contains(strings.Join(text, ""), "SCRAM") || strings.Contains(strings.Join(text, ""), "hunter2") {
		t.Fatalf("text: %s", text)
	}
}

func TestPGRoleSQLRefuses(t *testing.T) {
	argus := "argus-managed owner=me@example.com"
	bad := []PGAdminOp{
		{Kind: AdminRoleCreate, RoleSpec: &PGRoleSpec{Name: "pg_monitor"}},
		{Kind: AdminRoleCreate, RoleSpec: &PGRoleSpec{Name: "x", Comment: &argus}},
		{Kind: AdminRoleAlter, RoleSpec: &PGRoleSpec{Name: "x"}},
		{Kind: AdminRoleCreate, RoleSpec: &PGRoleSpec{Name: "x", ValidUntil: strp("tomorrow'; DROP")}},
		{Kind: AdminGrant, GrantSpec: &PGGrantSpec{Grantee: "x", On: OnAllTables, Schema: "s", Privileges: []string{"EXECUTE"}}},
		{Kind: AdminGrant, GrantSpec: &PGGrantSpec{Grantee: "x", On: OnTable, Schema: "s", Privileges: []string{"SELECT"}}},
		{Kind: AdminGrant, GrantSpec: &PGGrantSpec{Grantee: "x", On: OnAllTables, Schema: "s", Privileges: []string{"SELECT"}, Future: true}},
		{Kind: AdminGrant, GrantSpec: &PGGrantSpec{Grantee: "x", On: OnFunction, Schema: "s", Privileges: []string{"EXECUTE"},
			Objects: []string{"f(int); DROP TABLE t; --"}}},
		{Kind: AdminGrant, GrantSpec: &PGGrantSpec{Grantee: "x", On: OnFunction, Schema: "s", Privileges: []string{"EXECUTE"},
			Objects: []string{"f(int) TO x; GRANT ALL ON f(int"}}},
		{Kind: AdminGrant, GrantSpec: &PGGrantSpec{Grantee: "x", On: "everything", Privileges: []string{"ALL"}}},
		{Kind: AdminRoleMember, Member: &PGMemberSpec{Role: "a", Member: "a"}},
		{Kind: AdminRevoke, GrantSpec: &PGGrantSpec{Grantee: "x", On: OnAllTables, Schema: "s", Privileges: []string{"SELECT"}, SchemaUsage: true}},
	}
	for i, op := range bad {
		if got, err := PGRoleText(&op, "db"); err == nil {
			t.Errorf("%d (%s) was accepted: %s", i, op.Kind, got)
		}
	}
}

func strp(s string) *string { return &s }

func TestPGRoleAlwaysApproval(t *testing.T) {
	op := PGAdminOp{Kind: AdminRoleAlter, RoleSpec: &PGRoleSpec{Name: "x", Superuser: yes()}}
	if !PGRoleAlwaysApproval(&op) {
		t.Fatal("making a superuser did not always wait")
	}
	text, _ := PGRoleText(&op, "db")
	if !PGRoleTextMakesSuperuser(text) {
		t.Fatalf("its text is not recognised: %s", text)
	}
	op.RoleSpec.Superuser = no()
	if PGRoleAlwaysApproval(&op) {
		t.Fatal("taking superuser away always waits")
	}
	text, _ = PGRoleText(&op, "db")
	if PGRoleTextMakesSuperuser(text) || PGRoleTextMakesSuperuser(`DELETE FROM t WHERE x = ' WITH SUPERUSER'`) {
		t.Fatalf("not a superuser change, recognised as one: %s", text)
	}
}
