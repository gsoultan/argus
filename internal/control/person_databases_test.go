package control

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

// A database given to someone used to appear nowhere on their page: the
// "what can they do" window read server assignments only. This is the list
// it reads now.
func TestPersonDatabasesListsOwnAndTeamGrants(t *testing.T) {
	f := bulkFixture(t)
	ctx := context.Background()
	// ana: read on orders through the team, and her own write on orders that
	// needs approval, plus owner on billing of her own.
	if err := f.s.SetDBAssignment(ctx, DBAssignment{DatabaseID: f.orders.ID, TeamID: &f.team, Level: tunnel.LevelRead}, f.admin); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetDBAssignment(ctx, DBAssignment{DatabaseID: f.orders.ID, UserEmail: &f.ana, Level: tunnel.LevelWrite, NeedsApproval: true}, f.admin); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetDBAssignment(ctx, DBAssignment{DatabaseID: f.billing.ID, UserEmail: &f.ana, Level: tunnel.LevelOwner}, f.admin); err != nil {
		t.Fatal(err)
	}

	got, err := f.s.PersonDatabases(ctx, f.ana)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]PersonDatabase{}
	for _, d := range got {
		by[d.ID] = d
	}
	if len(by) != 2 {
		t.Fatalf("got %d databases, want orders and billing: %+v", len(by), got)
	}
	o := by[f.orders.ID]
	if o.Level != tunnel.LevelRead || o.Approvable != tunnel.LevelWrite || !o.Own || len(o.Teams) != 1 {
		t.Errorf("orders = %+v; want read from the team, write on approval of her own", o)
	}
	if b := by[f.billing.ID]; b.Level != tunnel.LevelOwner || !b.Own || len(b.Teams) != 0 {
		t.Errorf("billing = %+v; want her own owner", b)
	}

	// The window reads them through the access list: databases and brokers
	// apart, each with its words.
	w := do(t, f.h, http.MethodGet, "/api/v1/users/"+f.ana+"/access", "admin-token", "")
	var sections []AccessSection
	if err := json.Unmarshal(w.Body.Bytes(), &sections); err != nil || w.Code != http.StatusOK {
		t.Fatalf("access = %d %s", w.Code, w.Body)
	}
	for _, s := range sections {
		switch s.Kind {
		case "databases":
			if len(s.Items) != 2 {
				t.Errorf("databases section = %+v", s.Items)
			}
			for _, it := range s.Items {
				if it.ID == f.orders.ID && it.What != "Read, or Write with an approved request" {
					t.Errorf("orders reads %q", it.What)
				}
			}
		case "brokers":
			if len(s.Items) != 0 {
				t.Errorf("brokers section = %+v; ana has none", s.Items)
			}
		}
	}

	// bob has nothing.
	if got, _ := f.s.PersonDatabases(ctx, f.bob); len(got) != 0 {
		t.Errorf("bob = %+v, want nothing", got)
	}
}
