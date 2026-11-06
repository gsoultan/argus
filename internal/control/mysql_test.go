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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	_ "github.com/go-sql-driver/mysql"

	"github.com/gsoultan/argus/internal/agent"
	"github.com/gsoultan/argus/internal/tunnel"
)

/*
MySQL entries: what the control plane will save, and a session end to end --
ticket, relay, approval gate, agent, a real MySQL -- when
ARGUS_TEST_MYSQL_SOCKET names one whose root signs in over its socket (see
internal/agent/mysql_test.go).
*/

func TestMySQLEntries(t *testing.T) {
	_, s, _, pg, admin := dbFixture(t)
	ctx := context.Background()
	m, err := s.CreateDatabase(ctx, Database{AssetID: pg.AssetID, Engine: tunnel.EngineMySQL, Name: "shop"}, admin)
	if err != nil {
		t.Fatalf("mysql: %v", err)
	}
	if m.Host != "127.0.0.1" || m.Port != 3306 || m.ConfigPath != "" || m.Label != "production" || m.AdminUser != "" {
		t.Fatalf("mysql defaults: %+v", m)
	}
	// A SQL engine: production writes wait for approval, as on Postgres.
	if !m.WritesNeedApproval() || !m.SQL() {
		t.Fatalf("approval: %+v", m.Policy)
	}
	if _, err := s.CreateDatabase(ctx, Database{AssetID: pg.AssetID, Engine: tunnel.EngineMySQL, Name: "billing",
		Host: "/run/mysqld/mysqld.sock", ConfigPath: "/etc/argus/mysql.cnf"}, admin); err != nil {
		t.Fatalf("socket and option file: %v", err)
	}
	for _, bad := range []Database{
		{Engine: tunnel.EngineMySQL, Name: "mysql"},
		{Engine: tunnel.EngineMySQL, Name: "information_schema"},
		{Engine: tunnel.EngineMySQL, Name: "a/b"},
		{Engine: tunnel.EngineMySQL, Name: "x", Host: "db.internal"},
		{Engine: tunnel.EngineMySQL, Name: "y", ConfigPath: "/etc/shadow"},
		{Engine: tunnel.EngineMySQL, Name: "z", ConfigPath: "/etc/../root/.my.cnf"},
		{Engine: tunnel.EngineMySQL, Name: tunnel.TestDBPrefix + "x"},
	} {
		bad.AssetID = pg.AssetID
		if _, err := s.CreateDatabase(ctx, bad, admin); err == nil {
			t.Errorf("saved %+v", bad)
		} else if !errorsAs(err, new(ErrInvalidDatabase)) {
			t.Errorf("%+v: not an invalid-entry error: %v", bad, err)
		}
	}
}

func TestMySQLSessionThroughTheTunnel(t *testing.T) {
	sock := os.Getenv("ARGUS_TEST_MYSQL_SOCKET")
	if sock == "" {
		t.Skip("ARGUS_TEST_MYSQL_SOCKET is not set")
	}
	port := 3306
	if p := os.Getenv("ARGUS_TEST_MYSQL_PORT"); p != "" {
		port, _ = strconv.Atoi(p)
	}
	root, err := sql.Open("mysql", "root@unix("+sock+")/")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, q := range []string{"DROP DATABASE IF EXISTS argus_t_ctl", "CREATE DATABASE argus_t_ctl",
		"CREATE TABLE argus_t_ctl.items (id INT PRIMARY KEY)", "INSERT INTO argus_t_ctl.items VALUES (1),(2)"} {
		if _, err := root.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _, _ = root.Exec("DROP DATABASE IF EXISTS argus_t_ctl") })

	e := newTunnelEnv(t)
	e.withTerminal(t)
	host := unique("mysql-host")
	cred, _ := enrolled(t, e.store, e.h, host)
	ctx := context.Background()
	asset, err := e.store.CreateAsset(ctx, AssetInput{Hostname: host, Address: "192.0.2.21", Principals: []string{"ops"}})
	if err != nil {
		t.Fatal(err)
	}
	db, err := e.store.CreateDatabase(ctx, Database{AssetID: asset.ID, Engine: tunnel.EngineMySQL, Name: "argus_t_ctl",
		Host: sock, Port: port, Label: "production"}, e.api.UserTokens["admin-token"])
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	services := agent.DefaultTunnelServices()
	services[agent.MySQLService] = agent.MySQL(agent.MySQLConfig{StoreConfig: agent.StoreConfig{
		Verifier: &tunnel.GrantVerifier{Host: host}, RecordDir: t.TempDir(), Log: log}})
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
	if a := ask(tunnel.PGRequest{ID: 3, Op: "admin", Admin: &tunnel.PGAdminOp{Kind: tunnel.AdminExtensionList}}); a.Type != "error" {
		t.Fatalf("admin on mysql: %+v", a)
	}
	var n int
	_ = root.QueryRow("SELECT COUNT(*) FROM argus_t_ctl.items").Scan(&n)
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
			if proto != "mysql" || cnt == nil || *cnt != 1 {
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
