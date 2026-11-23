package tunnel

import (
	"strings"
	"testing"
)

func ownerGrant(engine, kind, name string) GrantCreate {
	c := GrantCreate{Op: "create", Engine: engine, Host: "127.0.0.1", Port: 5432, Name: "shop", Owner: &CreateOwner{Kind: kind, Name: name}}
	switch engine {
	case EnginePostgres:
		c.Postgres = &PGCreate{}
	case EngineMySQL:
		c.MySQL = &MySQLCreate{}
	case EngineMSSQL:
		c.MSSQL = &MSSQLCreate{}
	case EngineMongoDB:
		c.Mongo = &MongoCreate{Collection: "orders"}
	}
	return c
}

func TestOwnerPlanPerEngine(t *testing.T) {
	cases := []struct {
		engine string
		want   []string
	}{
		{EnginePostgres, []string{"CREATE ROLE shop_app LOGIN PASSWORD " + PasswordRedacted, "COMMENT ON ROLE shop_app"}},
		{EngineMySQL, []string{"CREATE USER 'shop_app'@'%' IDENTIFIED BY " + PasswordRedacted, "GRANT ALL PRIVILEGES ON `shop`.* TO 'shop_app'@'%'"}},
		{EngineMSSQL, []string{"CREATE LOGIN [shop_app] WITH PASSWORD = " + PasswordRedacted + " HASHED", "ALTER AUTHORIZATION ON DATABASE::[shop] TO [shop_app]"}},
		{EngineMongoDB, []string{`db.createUser({ user: "shop_app", pwd: ` + PasswordRedacted + `, roles: [{ role: "dbOwner", db: "shop" }] })`}},
	}
	for _, tc := range cases {
		c := ownerGrant(tc.engine, OwnerNew, "shop_app")
		if err := c.Validate(); err != nil {
			t.Fatalf("%s: %v", tc.engine, err)
		}
		plan := strings.Join(CreatePlan(c), "\n")
		for _, w := range tc.want {
			if !strings.Contains(plan, w) {
				t.Errorf("%s plan is missing %q:\n%s", tc.engine, w, plan)
			}
		}
		if CreateServiceFor(c) != CreateOwnedService || !c.NeedsPassword() {
			t.Errorf("%s: a new user must go to the owned service with a password", tc.engine)
		}
	}
}

func TestOwnerRefusals(t *testing.T) {
	for _, engine := range []string{EnginePostgres, EngineMySQL, EngineMSSQL, EngineMongoDB} {
		for _, name := range []string{"argus_owner", "Argus", "root", "postgres", "sa", "dbo", "a b", "x'; DROP", ""} {
			c := ownerGrant(engine, OwnerNew, name)
			if err := c.Validate(); err == nil {
				t.Errorf("%s accepted owner %q", engine, name)
			}
		}
	}
	c := ownerGrant(EnginePostgres, OwnerNew, "shop_app")
	for _, pw := range []string{"short", strings.Repeat("x", MaxOwnerPassword+1), "has\na-newline-in-it"} {
		if OwnerPasswordProblem(c, pw) == "" {
			t.Errorf("accepted password %q", pw)
		}
	}
	if why := OwnerPasswordProblem(c, "a-good-long-password"); why != "" {
		t.Errorf("refused a good password: %s", why)
	}
	m := ownerGrant(EngineMSSQL, OwnerNew, "shop_app")
	m.Owner.Policy = true
	if OwnerPasswordProblem(m, "alllowercaseletters") == "" {
		t.Error("SQL Server's policy accepted one kind of character")
	}
}

func TestCreateServiceFor(t *testing.T) {
	plain := GrantCreate{Op: "create", Engine: EnginePostgres, Name: "shop", Postgres: &PGCreate{}}
	if CreateServiceFor(plain) != CreateService {
		t.Error("a database with Argus's owner needs the owned service")
	}
	pgExisting := ownerGrant(EnginePostgres, OwnerExisting, "app")
	if CreateServiceFor(pgExisting) != CreateService {
		t.Error("an existing Postgres role must keep working with older agents")
	}
	if CreateServiceFor(ownerGrant(EngineMySQL, OwnerExisting, "app")) != CreateOwnedService {
		t.Error("a MySQL owner is new to agents")
	}
	if CreateServiceFor(GrantCreate{Op: CreateOpSetPassword, Engine: EnginePostgres}) != CreateOwnedService {
		t.Error("a new password is new to agents")
	}
}

func TestPoolerLines(t *testing.T) {
	if got := PoolerDatabaseLine("shop", "127.0.0.1", 5432); got != "shop = host=127.0.0.1 port=5432 dbname=shop" {
		t.Errorf("database line %q", got)
	}
	if got := PoolerUserLine(`a"b`, "SCRAM-SHA-256$x"); got != `"a""b" "SCRAM-SHA-256$x"` {
		t.Errorf("user line %q", got)
	}
}
