package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

// A SQL Server user, role or permission change, or a DBA tool's change,
// waits for approval where the database's changes do, for its own text with
// the password redacted; approved, it runs once and only as approved. Making
// someone db_owner, or simple recovery, waits on every database.
func TestMSSQLAccessChangeWaits(t *testing.T) {
	a, s, h, pg, admin := dbFixture(t)
	ctx := context.Background()
	db, err := s.CreateDatabase(ctx, Database{AssetID: pg.AssetID, Engine: tunnel.EngineMSSQL, Name: "sales"}, admin)
	if err != nil {
		t.Fatal(err)
	}
	ana, _ := newAccount(t, s, "operator")
	a.UserTokens["ana"] = ana
	if w := do(t, h, http.MethodPut, "/api/v1/databases/"+db.ID+"/assignments", "admin-token",
		fmt.Sprintf(`{"userEmail":%q,"level":"dba"}`, ana)); w.Code >= 300 {
		t.Fatalf("assign: %d %s", w.Code, w.Body)
	}
	gate := func(op tunnel.PGAdminOp, approval string) *tunnel.PGAnswer {
		return a.statementGate(ctx, ana, db, tunnel.LevelDBA, &tunnel.PGRequest{ID: 1, Op: "admin", Admin: &op, Approval: approval})
	}
	ask := func(text, why string) (int, string) {
		body, _ := json.Marshal(map[string]any{"items": []map[string]any{{"sql": text}}, "justification": why})
		w := do(t, h, http.MethodPost, "/api/v1/databases/"+db.ID+"/statements", "ana", string(body))
		var got StatementAsk
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if w.Code >= 300 {
			t.Logf("ask %q: %s", text, w.Body)
		}
		return w.Code, got.RequestID
	}
	grant := tunnel.PGAdminOp{Kind: tunnel.AdminMSGrant, MSGrant: &tunnel.MSSQLGrantSpec{Grantee: "reports",
		On: tunnel.MSOnSchema, Schema: "sales", Privileges: []string{"SELECT"}}}

	// Lists and previews never wait.
	if no := gate(tunnel.PGAdminOp{Kind: tunnel.AdminMSPrincipals}, ""); no != nil {
		t.Fatalf("a list waited: %+v", no)
	}
	pre := grant
	pre.Preview = true
	if no := gate(pre, ""); no != nil {
		t.Fatalf("a preview waited: %+v", no)
	}

	// Production: the grant waits, naming its own text, and runs once.
	no := gate(grant, "")
	if no == nil || no.Code != tunnel.GuardNeedsApproval || no.Approve != "GRANT SELECT ON SCHEMA::[sales] TO [reports]" {
		t.Fatalf("a production grant did not wait: %+v", no)
	}
	code, id := ask(no.Approve, "the reports job reads sales")
	if code != http.StatusCreated {
		t.Fatalf("ask: %d", code)
	}
	if _, err := s.DecideRequest(ctx, id, "approved", "", admin, false); err != nil {
		t.Fatal(err)
	}
	wider := grant
	wider.MSGrant = &tunnel.MSSQLGrantSpec{Grantee: "reports", On: tunnel.MSOnSchema, Schema: "sales", Privileges: []string{"SELECT", "DELETE"}}
	if no := gate(wider, id); no == nil {
		t.Fatal("a wider grant ran under a narrower approval")
	}
	if no := gate(grant, id); no != nil {
		t.Fatalf("the approved grant was stopped: %+v", no)
	}
	if no := gate(grant, id); no == nil {
		t.Fatal("an approval was used twice")
	}

	// The DBA tools: reads pass; a change waits for its own text.
	if no := gate(tunnel.PGAdminOp{Kind: tunnel.AdminMSActivity}, ""); no != nil {
		t.Fatalf("activity waited: %+v", no)
	}
	kill := tunnel.PGAdminOp{Kind: tunnel.AdminMSKill, MSDBA: &tunnel.MSSQLDBASpec{Session: 61}}
	if no := gate(kill, ""); no == nil || no.Approve != "KILL 61" {
		t.Fatalf("ending a session on production did not wait: %+v", no)
	}

	// A password is approved as [redacted].
	no = gate(tunnel.PGAdminOp{Kind: tunnel.AdminMSUserCreate, Principal: &tunnel.MSSQLPrincipalSpec{Name: "app",
		Auth: tunnel.MSAuthSQL, Password: "s3cret-for-app"}}, "")
	if no == nil || strings.Contains(no.Approve, "s3cret") || !strings.Contains(no.Approve, "PASSWORD = [redacted]") {
		t.Fatalf("new login: %+v", no)
	}

	// Staging runs without asking, except making someone db_owner.
	if w := do(t, h, http.MethodPatch, "/api/v1/databases/"+db.ID, "admin-token", `{"label":"staging","policy":{}}`); w.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", w.Code, w.Body)
	}
	db.Label = "staging"
	if no := gate(grant, ""); no != nil {
		t.Fatalf("a staging grant waited: %+v", no)
	}
	owner := tunnel.PGAdminOp{Kind: tunnel.AdminMSMember, Member: &tunnel.PGMemberSpec{Role: "db_owner", Member: "app"}}
	no = gate(owner, "")
	if no == nil || !strings.Contains(no.Error, "giving the whole of sales") {
		t.Fatalf("db_owner on staging did not wait: %+v", no)
	}
	if code, _ := ask(no.Approve, "the migration tool owns the schema"); code != http.StatusCreated {
		t.Fatalf("asking for db_owner on staging: %d", code)
	}
	if code, _ := ask("GRANT SELECT ON SCHEMA::[sales] TO [reports]", "no need"); code != http.StatusBadRequest {
		t.Fatalf("an ordinary grant ask on staging: %d", code)
	}
	if no := gate(kill, ""); no != nil {
		t.Fatalf("ending a session on staging waited: %+v", no)
	}
	simple := tunnel.PGAdminOp{Kind: tunnel.AdminMSDBSet, MSDBA: &tunnel.MSSQLDBASpec{Setting: "RECOVERY", Value: "SIMPLE"}}
	no = gate(simple, "")
	if no == nil || !strings.Contains(no.Error, "ends sales's log backups") || no.Approve != "ALTER DATABASE [sales] SET RECOVERY SIMPLE" {
		t.Fatalf("simple recovery on staging did not wait: %+v", no)
	}
	if code, _ := ask(no.Approve, "a scratch copy that needs no log backups"); code != http.StatusCreated {
		t.Fatalf("asking for simple recovery on staging: %d", code)
	}

	// Replication that ends what others depend on, or changes the server,
	// waits on staging too; adding an article does not.
	for _, c := range []struct {
		op   tunnel.PGAdminOp
		want string
	}{
		{tunnel.PGAdminOp{Kind: tunnel.AdminMSAGRemove, MSRepl: &tunnel.MSSQLReplSpec{Group: "ag1"}}, "ends its copies"},
		{tunnel.PGAdminOp{Kind: tunnel.AdminMSAGFailover, MSRepl: &tunnel.MSSQLReplSpec{Group: "ag1", Force: true}}, "moves the primary"},
		{tunnel.PGAdminOp{Kind: tunnel.AdminMSReplDistributor, MSRepl: &tunnel.MSSQLReplSpec{WorkingDir: "/var/opt/mssql/ReplData"}}, "whole server"},
		{tunnel.PGAdminOp{Kind: tunnel.AdminMSReplPubDrop, MSRepl: &tunnel.MSSQLReplSpec{Publication: "orders"}}, "stops its subscribers"},
	} {
		no := gate(c.op, "")
		if no == nil || !strings.Contains(no.Error, c.want) || no.Approve == "" || strings.Contains(no.Approve, "s3cret") {
			t.Fatalf("%s on staging: %+v", c.op.Kind, no)
		}
		if code, _ := ask(no.Approve, "planned maintenance window tonight"); code != http.StatusCreated {
			t.Fatalf("asking for %s on staging: %d", c.op.Kind, code)
		}
	}
	if no := gate(tunnel.PGAdminOp{Kind: tunnel.AdminMSReplArticleAdd, MSRepl: &tunnel.MSSQLReplSpec{Publication: "orders", Schema: "dbo", Table: "t"}}, ""); no != nil {
		t.Fatalf("adding an article on staging waited: %+v", no)
	}

	// Not SQL Server: refused rather than passed on.
	db.Engine = tunnel.EnginePostgres
	if no := gate(grant, ""); no == nil || !strings.Contains(no.Error, "SQL Server administration") {
		t.Fatalf("a Postgres access change: %+v", no)
	}
}
