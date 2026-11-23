//go:build !windows

package agent

import (
	"context"
	"slices"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

// A SQL Server database made through the create service: the options list
// the server's collations and default, the database gets the collation and
// recovery model asked for, and a name that is there is refused unchanged.
func TestMSSQLCreateDatabase(t *testing.T) {
	m := newMSService(t)
	pw, port := msTest(t)
	root := msRoot(t, pw, port)
	const name = "argus_t_created"
	drop := func() { _, _ = root.Exec("IF DB_ID(N'" + name + "') IS NOT NULL DROP DATABASE [" + name + "]") }
	drop()
	t.Cleanup(drop)
	ctx := context.Background()
	c := tunnel.GrantCreate{Op: tunnel.CreateOpOptions, Engine: tunnel.EngineMSSQL, Host: "127.0.0.1", Port: port,
		ConfigPath: m.file, Name: "x"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	opts := createOn(ctx, CreateDBConfig{}, c, "ana@corp.example", "")
	if !opts.OK || opts.Options == nil || opts.Options.DefaultCollation == "" || opts.Options.DefaultRecovery == "" ||
		!slices.Contains(opts.Options.Collations, "Latin1_General_100_CI_AS_SC_UTF8") || opts.Version == "" {
		t.Fatalf("options: %+v %+v", opts, opts.Options)
	}

	c = tunnel.GrantCreate{Op: tunnel.CreateOpCreate, Engine: tunnel.EngineMSSQL, Host: "127.0.0.1", Port: port,
		ConfigPath: m.file, Name: name, MSSQL: &tunnel.MSSQLCreate{Collation: "Latin1_General_100_CI_AS_SC_UTF8", Recovery: "simple"}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	res := createOn(ctx, CreateDBConfig{}, c, "ana@corp.example", "")
	if !res.OK || len(res.Statements) != 2 {
		t.Fatalf("create: %+v", res)
	}
	var coll, rec string
	if err := root.QueryRow("SELECT collation_name, recovery_model_desc FROM sys.databases WHERE name = @p1", name).Scan(&coll, &rec); err != nil ||
		coll != "Latin1_General_100_CI_AS_SC_UTF8" || rec != "SIMPLE" {
		t.Fatalf("made: %v %s %s", err, coll, rec)
	}
	if again := createOn(ctx, CreateDBConfig{}, c, "ana@corp.example", ""); again.OK || !again.Exists {
		t.Fatalf("a second create: %+v", again)
	}
	bad := c
	bad.Name, bad.MSSQL = "argus_t_nocoll", &tunnel.MSSQLCreate{Collation: "No_Such_Collation"}
	if r := createOn(ctx, CreateDBConfig{}, bad, "ana@corp.example", ""); r.OK || r.Error != "this server has no collation No_Such_Collation" {
		t.Fatalf("an unknown collation: %+v", r)
	}
}
