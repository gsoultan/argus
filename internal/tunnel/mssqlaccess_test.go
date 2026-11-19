package tunnel

import (
	"strings"
	"testing"
)

func TestMSSQLAccessText(t *testing.T) {
	str := func(s string) *string { return &s }
	no := false
	for _, c := range []struct {
		op   PGAdminOp
		want string
	}{
		{PGAdminOp{Kind: AdminMSUserCreate, Principal: &MSSQLPrincipalSpec{Name: "app", Auth: MSAuthSQL, Password: "s3cret!",
			DefaultSchema: str("sales"), MemberOf: []string{"db_datareader", "public"}}},
			"CREATE LOGIN [app] WITH PASSWORD = [redacted], DEFAULT_DATABASE = [shop], CHECK_EXPIRATION = OFF, CHECK_POLICY = OFF;\n" +
				"CREATE USER [app] FOR LOGIN [app] WITH DEFAULT_SCHEMA = [sales];\n" +
				"ALTER ROLE [db_datareader] ADD MEMBER [app]"},
		{PGAdminOp{Kind: AdminMSUserCreate, Principal: &MSSQLPrincipalSpec{Name: "ops", Auth: MSAuthLogin, Login: `CORP\ops`}},
			`CREATE USER [ops] FOR LOGIN [CORP\ops]`},
		{PGAdminOp{Kind: AdminMSUserCreate, Principal: &MSSQLPrincipalSpec{Name: "runner", Auth: MSAuthNone}},
			"CREATE USER [runner] WITHOUT LOGIN"},
		{PGAdminOp{Kind: AdminMSUserAlter, Principal: &MSSQLPrincipalSpec{Name: "app", Login: "app", Password: "x",
			Enabled: &no, DefaultSchema: str(""), Rename: "app2"}},
			"ALTER LOGIN [app] WITH PASSWORD = [redacted];\nALTER LOGIN [app] DISABLE;\n" +
				"ALTER USER [app] WITH DEFAULT_SCHEMA = NULL;\nALTER USER [app] WITH NAME = [app2]"},
		{PGAdminOp{Kind: AdminMSUserDrop, Principal: &MSSQLPrincipalSpec{Name: "app", Login: "app", DropLogin: true}},
			"DROP USER [app];\nDROP LOGIN [app]"},
		{PGAdminOp{Kind: AdminMSRoleCreate, Principal: &MSSQLPrincipalSpec{Name: "we]ird"}}, "CREATE ROLE [we]]ird]"},
		{PGAdminOp{Kind: AdminMSRoleAlter, Principal: &MSSQLPrincipalSpec{Name: "a", Rename: "b"}}, "ALTER ROLE [a] WITH NAME = [b]"},
		{PGAdminOp{Kind: AdminMSMember, Member: &PGMemberSpec{Role: "db_datawriter", Member: "app"}},
			"ALTER ROLE [db_datawriter] ADD MEMBER [app]"},
		{PGAdminOp{Kind: AdminMSUnmember, Member: &PGMemberSpec{Role: "r", Member: "app"}}, "ALTER ROLE [r] DROP MEMBER [app]"},
		{PGAdminOp{Kind: AdminMSGrant, MSGrant: &MSSQLGrantSpec{Grantee: "app", On: MSOnDatabase,
			Privileges: []string{"SHOWPLAN", "SELECT"}, WithGrant: true}},
			"GRANT SELECT, SHOWPLAN ON DATABASE::[shop] TO [app] WITH GRANT OPTION"},
		{PGAdminOp{Kind: AdminMSRevoke, MSGrant: &MSSQLGrantSpec{Grantee: "public", On: MSOnSchema, Schema: "sales",
			Privileges: []string{"SELECT"}, WithGrant: true}},
			"REVOKE GRANT OPTION FOR SELECT ON SCHEMA::[sales] FROM [public] CASCADE"},
		{PGAdminOp{Kind: AdminMSDeny, MSGrant: &MSSQLGrantSpec{Grantee: "app", On: MSOnObject, Schema: "dbo",
			Objects: []string{"a", "b"}, Privileges: []string{"DELETE"}}},
			"DENY DELETE ON OBJECT::[dbo].[a] TO [app];\nDENY DELETE ON OBJECT::[dbo].[b] TO [app]"},
	} {
		got, err := MSSQLAccessText(&c.op, "shop")
		if err != nil || got != c.want {
			t.Errorf("%s:\n got %q (%v)\nwant %q", c.op.Kind, got, err, c.want)
		}
	}
}

func TestMSSQLAccessRefusals(t *testing.T) {
	for _, c := range []struct {
		op   PGAdminOp
		want string
	}{
		{PGAdminOp{Kind: AdminMSUserDrop, Principal: &MSSQLPrincipalSpec{Name: "argus-ana-read-0a1b2c3d"}}, "Argus's own"},
		{PGAdminOp{Kind: AdminMSUserCreate, Principal: &MSSQLPrincipalSpec{Name: "x", Auth: MSAuthLogin, Login: "ARGUS-ana-dba-1"}}, "Argus's own"},
		{PGAdminOp{Kind: AdminMSRoleDrop, Principal: &MSSQLPrincipalSpec{Name: "db_owner"}}, "fixed roles"},
		{PGAdminOp{Kind: AdminMSUserDrop, Principal: &MSSQLPrincipalSpec{Name: "dbo"}}, "SQL Server's own"},
		{PGAdminOp{Kind: AdminMSUserCreate, Principal: &MSSQLPrincipalSpec{Name: "x", Auth: MSAuthSQL}}, "needs a password"},
		{PGAdminOp{Kind: AdminMSUserCreate, Principal: &MSSQLPrincipalSpec{Name: "x"}}, "how does the user sign in"},
		{PGAdminOp{Kind: AdminMSUserCreate, Principal: &MSSQLPrincipalSpec{Name: " x", Auth: MSAuthNone}}, "space"},
		{PGAdminOp{Kind: AdminMSUserCreate, Principal: &MSSQLPrincipalSpec{Name: "x\ny", Auth: MSAuthNone}}, "control"},
		{PGAdminOp{Kind: AdminMSUserAlter, Principal: &MSSQLPrincipalSpec{Name: "x", Password: "p"}}, "the login"},
		{PGAdminOp{Kind: AdminMSUserAlter, Principal: &MSSQLPrincipalSpec{Name: "x"}}, "nothing to change"},
		{PGAdminOp{Kind: AdminMSMember, Member: &PGMemberSpec{Role: "r", Member: "dbo"}}, "SQL Server's own"},
		{PGAdminOp{Kind: AdminMSMember, Member: &PGMemberSpec{Role: "r", Member: "r"}}, "itself"},
		{PGAdminOp{Kind: AdminMSGrant, MSGrant: &MSSQLGrantSpec{Grantee: "a", On: MSOnObject, Schema: "dbo",
			Objects: []string{"t"}, Privileges: []string{"CREATE TABLE"}}}, "not a permission"},
		{PGAdminOp{Kind: AdminMSGrant, MSGrant: &MSSQLGrantSpec{Grantee: "a", On: "server", Privileges: []string{"CONTROL SERVER"}}}, "cannot grant"},
		{PGAdminOp{Kind: AdminMSDeny, MSGrant: &MSSQLGrantSpec{Grantee: "a", On: MSOnDatabase, Privileges: []string{"SELECT"}, WithGrant: true}}, "grant option"},
		{PGAdminOp{Kind: AdminMSGrant, MSGrant: &MSSQLGrantSpec{Grantee: "a", On: MSOnObject, Schema: "dbo", Objects: []string{"t]; DROP TABLE x --"},
			Privileges: []string{"SELECT"}}}, ""},
	} {
		text, err := MSSQLAccessText(&c.op, "shop")
		if c.want == "" {
			// Not refused, and quoted: the name stays one name.
			if err != nil || !strings.Contains(text, "[t]]; DROP TABLE x --]") {
				t.Errorf("%s: %q %v", c.op.Kind, text, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %+v: %v, want %q", c.op.Kind, c.op, err, c.want)
		}
	}
}

func TestMSSQLAlwaysApproval(t *testing.T) {
	for _, c := range []struct {
		op   PGAdminOp
		want bool
	}{
		{PGAdminOp{Kind: AdminMSMember, Member: &PGMemberSpec{Role: "DB_OWNER", Member: "app"}}, true},
		{PGAdminOp{Kind: AdminMSMember, Member: &PGMemberSpec{Role: "db_datareader", Member: "app"}}, false},
		{PGAdminOp{Kind: AdminMSRoleCreate, Principal: &MSSQLPrincipalSpec{Name: "r", MemberOf: []string{"db_owner"}}}, true},
		{PGAdminOp{Kind: AdminMSGrant, MSGrant: &MSSQLGrantSpec{Grantee: "a", On: MSOnDatabase, Privileges: []string{"CONTROL"}}}, true},
		{PGAdminOp{Kind: AdminMSGrant, MSGrant: &MSSQLGrantSpec{Grantee: "a", On: MSOnSchema, Schema: "s", Privileges: []string{"CONTROL"}}}, false},
		{PGAdminOp{Kind: AdminMSDeny, MSGrant: &MSSQLGrantSpec{Grantee: "a", On: MSOnDatabase, Privileges: []string{"CONTROL"}}}, false},
	} {
		if got := MSSQLAlwaysApproval(&c.op); got != c.want {
			t.Errorf("%s: %v, want %v", c.op.Kind, got, c.want)
		}
		text, err := MSSQLAccessText(&c.op, "shop")
		if err != nil {
			t.Fatal(err)
		}
		if got := MSSQLTextAlwaysApproval(text); got != c.want {
			t.Errorf("text %q: %v, want %v", text, got, c.want)
		}
	}
}

// The hash's shape: version 0x0200, the salt, and 64 bytes of SHA-512. The
// agent's TestMSSQLPasswordHash checks SQL Server accepts it.
func TestMSSQLPasswordHashFormat(t *testing.T) {
	h := mssqlHashWithSalt("a", []byte{1, 2, 3, 4})
	if !strings.HasPrefix(h, "0x020001020304") || len(h) != 2+4+8+128 {
		t.Fatalf("hash: %s", h)
	}
	r := (&PGAdminOp{Kind: AdminMSUserCreate, Principal: &MSSQLPrincipalSpec{Name: "x", Password: "pw"}}).Redacted()
	if r.Principal.Password != PasswordRedacted {
		t.Fatal("not redacted")
	}
}
