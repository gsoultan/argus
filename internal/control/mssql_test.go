package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	_ "github.com/microsoft/go-mssqldb"

	"github.com/gsoultan/argus/internal/agent"
	"github.com/gsoultan/argus/internal/tunnel"
)

/*
SQL Server entries: what the control plane will save, and a session end to
end -- ticket, relay, approval gate, agent, a real SQL Server -- when
ARGUS_TEST_MSSQL_SA_PASSWORD holds the sa password of one on 127.0.0.1 (see
internal/agent/mssql_test.go).
*/

func TestMSSQLEntries(t *testing.T) {
	_, s, _, pg, admin := dbFixture(t)
	ctx := context.Background()
	m, err := s.CreateDatabase(ctx, Database{AssetID: pg.AssetID, Engine: tunnel.EngineMSSQL, Name: "Sales DB"}, admin)
	if err != nil {
		t.Fatalf("mssql: %v", err)
	}
	if m.Host != "127.0.0.1" || m.Port != 1433 || m.ConfigPath != "" || m.Label != "production" || m.AdminUser != "" {
		t.Fatalf("mssql defaults: %+v", m)
	}
	// A SQL engine: production writes wait for approval, as on Postgres.
	if !m.WritesNeedApproval() || !m.SQL() {
		t.Fatalf("approval: %+v", m.Policy)
	}
	if _, err := s.CreateDatabase(ctx, Database{AssetID: pg.AssetID, Engine: tunnel.EngineMSSQL, Name: "billing",
		Port: 14330, ConfigPath: "/etc/argus/mssql-2.conf"}, admin); err != nil {
		t.Fatalf("port and admin file: %v", err)
	}
	for _, bad := range []Database{
		{Engine: tunnel.EngineMSSQL, Name: "master"},
		{Engine: tunnel.EngineMSSQL, Name: "tempdb"},
		{Engine: tunnel.EngineMSSQL, Name: "x", Host: "db.internal"},
		{Engine: tunnel.EngineMSSQL, Name: "x2", Host: "/var/opt/mssql"},
		{Engine: tunnel.EngineMSSQL, Name: "y", ConfigPath: "/etc/shadow"},
		{Engine: tunnel.EngineMSSQL, Name: "z", ConfigPath: "/etc/../root/mssql.conf"},
		{Engine: tunnel.EngineMSSQL, Name: tunnel.TestDBPrefix + "x"},
	} {
		bad.AssetID = pg.AssetID
		if _, err := s.CreateDatabase(ctx, bad, admin); err == nil {
			t.Errorf("saved %+v", bad)
		} else if !errorsAs(err, new(ErrInvalidDatabase)) {
			t.Errorf("%+v: not an invalid-entry error: %v", bad, err)
		}
	}
}

func TestMSSQLSessionThroughTheTunnel(t *testing.T) {
	pw := os.Getenv("ARGUS_TEST_MSSQL_SA_PASSWORD")
	if pw == "" {
		t.Skip("ARGUS_TEST_MSSQL_SA_PASSWORD is not set")
	}
	port := 1433
	if p := os.Getenv("ARGUS_TEST_MSSQL_PORT"); p != "" {
		port, _ = strconv.Atoi(p)
	}
	u0 := url.URL{Scheme: "sqlserver", User: url.UserPassword("sa", pw), Host: "127.0.0.1:" + strconv.Itoa(port),
		RawQuery: "database=master&encrypt=false&TrustServerCertificate=true"}
	root, err := sql.Open("sqlserver", u0.String())
	if err != nil {
		t.Fatal(err)
	}
	root.SetMaxOpenConns(1)
	defer root.Close()
	drop := "IF DB_ID('argus_t_ctl') IS NOT NULL BEGIN ALTER DATABASE argus_t_ctl SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE argus_t_ctl END"
	for _, q := range []string{drop, "CREATE DATABASE argus_t_ctl",
		"CREATE TABLE argus_t_ctl.dbo.items (id INT PRIMARY KEY)", "INSERT INTO argus_t_ctl.dbo.items VALUES (1),(2)"} {
		if _, err := root.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _, _ = root.Exec(drop) })
	file := filepath.Join(t.TempDir(), "mssql.conf")
	if err := os.WriteFile(file, []byte("user = sa\npassword = "+pw+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	e := newTunnelEnv(t)
	e.withTerminal(t)
	host := unique("mssql-host")
	cred, _ := enrolled(t, e.store, e.h, host)
	ctx := context.Background()
	asset, err := e.store.CreateAsset(ctx, AssetInput{Hostname: host, Address: "192.0.2.22", Principals: []string{"ops"}})
	if err != nil {
		t.Fatal(err)
	}
	db, err := e.store.CreateDatabase(ctx, Database{AssetID: asset.ID, Engine: tunnel.EngineMSSQL, Name: "argus_t_ctl",
		Port: port, ConfigPath: file, Label: "production"}, e.api.UserTokens["admin-token"])
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	services := agent.DefaultTunnelServices()
	services[agent.MSSQLService] = agent.MSSQL(agent.StoreConfig{
		Verifier: &tunnel.GrantVerifier{Host: host}, RecordDir: t.TempDir(), Log: log})
	actx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go agent.RunTunnel(actx, agent.TunnelConfig{Dial: e.dial(cred, host), Services: services, Log: log})
	e.waitTunnel(t, host, "")

	w := do(t, e.h, http.MethodPost, "/api/v1/databases/"+db.ID+"/ticket", "admin-token", `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("ticket: %d %s", w.Code, w.Body)
	}
	var tk struct{ Ticket, Level, Role string }
	_ = json.Unmarshal(w.Body.Bytes(), &tk)
	u, _ := url.Parse(e.srv.URL)
	u.Scheme, u.Path = "wss", "/api/v1/postgres"
	u.RawQuery = url.Values{"database": {db.ID}, "principal": {tk.Role + "@" + tk.Level}, "ticket": {tk.Ticket}}.Encode()
	wctx, wcancel := context.WithTimeout(ctx, 20*time.Second)
	defer wcancel()
	c, _, err := websocket.Dial(wctx, u.String(), &websocket.DialOptions{HTTPClient: e.srv.Client(),
		HTTPHeader: http.Header{"Origin": {e.srv.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	read := func() pgBrowserMessage {
		t.Helper()
		for {
			typ, p, err := c.Read(wctx)
			if err != nil {
				t.Fatal(err)
			}
			if typ == websocket.MessageBinary {
				continue // row batches
			}
			var m pgBrowserMessage
			_ = json.Unmarshal(p, &m)
			return m
		}
	}
	if m := read(); m.Type != "ready" {
		t.Fatalf("not ready: %+v", m)
	}
	var ready tunnel.PGAnswer
	if m := read(); json.Unmarshal(m.Ans, &ready) != nil || ready.Type != "ready" || ready.Level != "dba" ||
		!strings.HasPrefix(ready.Role, "argus-") {
		t.Fatalf("ready answer: %+v", ready)
	}
	ask := func(req tunnel.PGRequest) tunnel.PGAnswer {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"req": req})
		if err := c.Write(wctx, websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
		for {
			var a tunnel.PGAnswer
			_ = json.Unmarshal(read().Ans, &a)
			if a.ID == req.ID && a.Type != "cols" {
				return a
			}
		}
	}
	if a := ask(tunnel.PGRequest{ID: 1, Op: "run", SQL: "SELECT * FROM items"}); a.Type != "done" || a.Tag != "SELECT 2" {
		t.Fatalf("select: %+v", a)
	}
	// Production: the control plane holds a write for approval before the
	// agent sees it.
	if a := ask(tunnel.PGRequest{ID: 2, Op: "run", SQL: "INSERT INTO items VALUES (3)"}); a.Code != tunnel.GuardNeedsApproval {
		t.Fatalf("a production write ran without approval: %+v", a)
	}
	// A batch that starts with a read and then writes is a write.
	if a := ask(tunnel.PGRequest{ID: 4, Op: "run", SQL: "SELECT 1 DELETE FROM items WHERE id = 1"}); a.Code != tunnel.GuardNeedsApproval {
		t.Fatalf("a write behind a read ran without approval: %+v", a)
	}
	if a := ask(tunnel.PGRequest{ID: 5, Op: "run", SQL: "EXEC sp_rename 'items', 'gone'"}); a.Code != tunnel.GuardNeedsApproval {
		t.Fatalf("a procedure ran without approval: %+v", a)
	}
	if a := ask(tunnel.PGRequest{ID: 3, Op: "admin", Admin: &tunnel.PGAdminOp{Kind: tunnel.AdminExtensionList}}); a.Type != "error" {
		t.Fatalf("admin on sql server: %+v", a)
	}
	var n int
	_ = root.QueryRow("SELECT COUNT(*) FROM argus_t_ctl.dbo.items").Scan(&n)
	if n != 2 {
		t.Fatalf("the held write ran: %d rows", n)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")

	deadline := time.Now().Add(10 * time.Second)
	for {
		var proto, state string
		var cnt *int
		err := e.store.pool.QueryRow(ctx, `SELECT protocol, state, command_count FROM sessions WHERE database_id = $1::uuid`,
			db.ID).Scan(&proto, &state, &cnt)
		if err == nil && state == "ended" {
			if proto != "mssql" || cnt == nil || *cnt != 1 {
				t.Fatalf("session row: %s %v", proto, cnt)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the session never ended: %v %s", err, state)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
