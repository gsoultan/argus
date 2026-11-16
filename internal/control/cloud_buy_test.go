package control

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"testing"
)

func TestTotals(t *testing.T) {
	for f, want := range map[float64]string{3592.4: "$3,592", 1042.85: "$1,043", 99.5: "$99.50", 1234567: "$1,234,567"} {
		if got := totalText(f); got != want {
			t.Fatalf("%v: %s", f, got)
		}
	}
	if !sameTotal("$3,592", 3592.4) || !sameTotal("3592", 3591.6) || sameTotal("3,500", 3592.4) || sameTotal("three", 1) || !sameTotal("$99.50", 99.5) {
		t.Fatal("typed totals")
	}
}

// Buying a commitment: quoted, the total typed, always someone else's
// approval (even in Development), the asker's only when nobody else could
// decide, and re-priced just before buying.
func TestCloudBuyReservation(t *testing.T) {
	e := changeAPI(t)
	region := "ap-southeast-3"
	riQuery := `{"kind":"ri","size":"m6i.large","count":2,"termMonths":12,"payment":"No Upfront","region":"` + region + `"}`

	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/reservations/quote", "auditor-token", `{"accountId":"`+e.acct.ID+`","query":`+riQuery+`}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("an auditor quotes: %d", w.Code)
	}
	w = do(t, e.h, http.MethodPost, "/api/v1/cloud/reservations/quote", "admin-token", `{"accountId":"`+e.acct.ID+`","query":`+riQuery+`}`)
	if w.Code != http.StatusOK {
		t.Fatalf("quote: %d %s", w.Code, w.Body)
	}
	var q cloudQuoteView
	_ = json.Unmarshal(w.Body.Bytes(), &q)
	want := 0.096 * 0.62 * 2 * 8760
	if math.Abs(q.Offer.Total-want) > 0.02 || q.TotalText != totalText(q.Offer.Total) || q.Saves <= 0 || q.PaysOffMonth != 1 ||
		!q.AlwaysApproval || len(q.Covers) != 1 || q.Covers[0] != "pay-api-01" {
		t.Fatalf("quote: %+v", q)
	}

	// Development, where nothing else waits: a buy still does.
	if _, err := e.s.pool.Exec(context.Background(), `UPDATE cloud_accounts SET environment = 'development' WHERE id = $1`, e.acct.ID); err != nil {
		t.Fatal(err)
	}
	body := func(action, spec, confirm, reason string) string {
		return `{"accountId":"` + e.acct.ID + `","region":"` + region + `","action":"` + action + `","spec":` + spec +
			`,"confirm":"` + confirm + `","reason":"` + reason + `"}`
	}
	e.ask(t, "admin-token", body("ri-buy", riQuery, "$1,000", "steady web tier"), http.StatusConflict)
	e.ask(t, "admin-token", body("ri-buy", riQuery, q.TotalText, ""), http.StatusBadRequest)
	e.ask(t, "admin-token", body("sp-buy", riQuery, q.TotalText, "steady web tier"), http.StatusBadRequest)
	c := e.ask(t, "admin-token", body("ri-buy", riQuery, q.TotalText, "steady web tier"), http.StatusAccepted)
	if c.State != "awaiting_approval" || c.Level != "remove" || c.Environment != "development" ||
		!strings.Contains(c.Summary, "Buy a Reserved Instance for 2 m6i.large in ap-southeast-3, 1 year, no upfront: "+q.TotalText+" in all") {
		t.Fatalf("asked: %+v", c)
	}
	// Not the person who asked, while someone else could decide.
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes/"+c.ID+"/approve", "admin-token", `{"confirmSelf":"approve my own request"}`); w.Code != http.StatusForbidden ||
		!strings.Contains(w.Body.String(), "someone other than the person who asked") {
		t.Fatalf("own approval: %d %s", w.Code, w.Body)
	}
	done := e.decide(t, "approver-token", c.ID, true)
	if done.State != "applied" || len(e.fake.Bought) != 1 {
		t.Fatalf("bought: %+v %v", done, e.fake.Bought)
	}

	// The price moves between asking and approving: nothing is bought.
	sp := `{"kind":"sp","planType":"Compute","commitment":0.5,"termMonths":36,"payment":"Partial Upfront","region":"` + region + `"}`
	w = do(t, e.h, http.MethodPost, "/api/v1/cloud/reservations/quote", "admin-token", `{"accountId":"`+e.acct.ID+`","query":`+sp+`}`)
	_ = json.Unmarshal(w.Body.Bytes(), &q)
	if q.Offer.Total != 13140 || q.Offer.Upfront != 6570 || q.OnDemand != 0 || len(q.Covers) != 0 {
		t.Fatalf("sp quote: %+v", q)
	}
	c = e.ask(t, "admin-token", body("sp-buy", sp, "$13,140", "compute baseline"), http.StatusAccepted)
	e.fake.PriceBump = 0.01
	moved := e.decide(t, "approver-token", c.ID, true)
	if moved.State != "failed" || !strings.Contains(moved.Error, "price changed") || len(e.fake.Bought) != 1 {
		t.Fatalf("moved: %+v %v", moved, e.fake.Bought)
	}
	// And asking at the old total is refused now.
	e.ask(t, "admin-token", body("sp-buy", sp, "$13,140", "compute baseline"), http.StatusConflict)

	// Alone in the organization, the person who asked approves their own
	// purchase with the phrase, and the audit log says so.
	e.fake.PriceBump = 0
	ctx := context.Background()
	if _, err := e.s.pool.Exec(ctx, `UPDATE users SET disabled_at = now() WHERE disabled_at IS NULL AND role = ANY($1) AND lower(email) <> lower($2)`,
		requestDeciders, e.admin); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.s.pool.Exec(context.Background(), `UPDATE users SET disabled_at = NULL WHERE role = ANY($1) AND lower(email) <> lower($2)`, requestDeciders, e.admin)
	})
	w = do(t, e.h, http.MethodPost, "/api/v1/cloud/reservations/quote", "admin-token", `{"accountId":"`+e.acct.ID+`","query":`+riQuery+`}`)
	_ = json.Unmarshal(w.Body.Bytes(), &q)
	c = e.ask(t, "admin-token", body("ri-buy", riQuery, q.TotalText, "steady web tier"), http.StatusAccepted)
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes/"+c.ID+"/approve", "admin-token", `{}`); w.Code != http.StatusConflict {
		t.Fatalf("alone, without the phrase: %d %s", w.Code, w.Body)
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes/"+c.ID+"/approve", "admin-token", `{"confirmSelf":"approve my own request"}`); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), `"applied"`) || len(e.fake.Bought) != 2 {
		t.Fatalf("alone, own purchase: %d %s %v", w.Code, w.Body, e.fake.Bought)
	}
}
