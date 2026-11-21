package control

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// Every kind the engine knows is one the table accepts, and the other way
// round: a kind added to one and not the other is a check that can be made
// and never run, or run and never shown.
func TestCheckKindsMatchTheTable(t *testing.T) {
	h := newTenantHarness(t)
	ctx := context.Background()
	var def string
	if err := h.store.db(ctx).QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conname = 'checks_kind_check'`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	var inTable []string
	for _, part := range strings.Split(def, "'")[1:] {
		if !strings.HasPrefix(part, ":") && !strings.HasPrefix(part, ")") && !strings.HasPrefix(part, ",") && !strings.HasPrefix(part, "]") {
			inTable = append(inTable, part)
		}
	}
	var inCode []string
	for _, k := range checkKinds {
		if k.Name == "" || k.Label == "" {
			t.Errorf("a kind needs a name and a label: %+v", k)
		}
		inCode = append(inCode, k.Name)
	}
	slices.Sort(inTable)
	slices.Sort(inCode)
	if !slices.Equal(inTable, inCode) {
		t.Fatalf("checks_kind_check allows %v; checkKinds has %v (%s)", inTable, inCode, def)
	}
}

// The website checks an organization had before the shared engine are
// still there after it, as they were: settings, sealed headers, where they
// stood, their rounds (so uptime is unchanged) and their outages, open or
// over. Then the next round and the next outage carry on from them.
func TestWebsiteChecksMoveToTheSharedEngineIntact(t *testing.T) {
	names, err := migrationNames()
	if err != nil {
		t.Fatal(err)
	}
	at := slices.Index(names, "103_checks.sql")
	if at < 0 {
		t.Fatal("no 103_checks.sql")
	}
	ctx := context.Background()
	dsn := scratchDB(t, fmt.Sprintf("argus_checks_%d", time.Now().UnixNano()%100000))
	s := openAt(t, dsn, at)
	now := time.Now().UTC().Truncate(time.Second)
	var id string
	if err := s.pool.QueryRow(ctx, `INSERT INTO website_checks (name, url, expect_status, keyword, every_seconds,
		timeout_seconds, created_by, method, request_body, header_names, headers_sealed, state, state_since,
		failing_rounds, last_at, last_status, last_detail, last_outcome, cert_not_after, telegram, telegram_bot, telegram_chats)
		VALUES ('Shop', 'https://shop.example.com/health', 200, 'ok', 300, 20, 'admin@example.com', 'POST', '{"ping":1}',
		        '{Authorization}', '\x0102'::bytea, 'down', $1, 2, $1, 503, 'HTTP 503', 'down', $2, 'chats', 'platform', '{-100}')
		RETURNING id::text`, now.Add(-time.Hour), now.Add(30*24*time.Hour)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for i, outcome := range []string{"up", "up", "down"} {
		if _, err := s.pool.Exec(ctx, `INSERT INTO website_rounds (check_id, at, secs, outcome, status)
			VALUES ($1::uuid, $2, 300, $3, 200)`, id, now.Add(time.Duration(i-3)*5*time.Minute), outcome); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO website_outages (check_id, started_at, ended_at, cause) VALUES
		($1::uuid, $2, $3, 'earlier'), ($1::uuid, $4, NULL, 'HTTP 503')`,
		id, now.Add(-48*time.Hour), now.Add(-47*time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer s2.Close()

	c, err := s2.WebsiteCheck(ctx, id)
	if err != nil {
		t.Fatalf("the check is gone after the upgrade: %v", err)
	}
	if c.Name != "Shop" || c.URL != "https://shop.example.com/health" || c.ExpectStatus != 200 || c.Keyword != "ok" ||
		c.EverySeconds != 300 || c.TimeoutSeconds != 20 || c.Method != "POST" || c.Body != `{"ping":1}` ||
		!slices.Equal(c.HeaderNames, []string{"Authorization"}) || string(c.headersSealed) != "\x01\x02" {
		t.Errorf("settings changed: %+v", c)
	}
	if c.State != "down" || c.FailingRounds != 2 || c.LastStatus == nil || *c.LastStatus != 503 || c.LastOutcome != "down" ||
		c.CertNotAfter == nil || !c.CertNotAfter.Equal(now.Add(30*24*time.Hour)) {
		t.Errorf("where it stood changed: %+v", c)
	}
	if c.Telegram != "chats" || c.TelegramBot != "platform" || !slices.Equal(c.TelegramChats, []string{"-100"}) {
		t.Errorf("its Telegram choice changed: %+v", c)
	}
	rounds, err := s2.WebsiteRounds(ctx, id, now.Add(-time.Hour))
	if err != nil || len(rounds) != 3 {
		t.Fatalf("rounds %v, %v", rounds, err)
	}
	up, err := s2.WebsiteUptimes(ctx, now)
	if err != nil || up[id].Day == nil {
		t.Fatalf("uptime %v, %v", up, err)
	}
	if got := *up[id].Day; got < 66 || got > 67 {
		t.Errorf("uptime %.1f%%, want two rounds in three", got)
	}
	outages, err := s2.WebsiteOutages(ctx, id, 10)
	if err != nil || len(outages) != 2 {
		t.Fatalf("outages %v, %v", outages, err)
	}

	// The open outage ends when it comes back, and a new one gets an id of
	// its own rather than one the copied outages already hold.
	tr, err := s2.RecordWebsiteRound(ctx, id, websiteRoundResult{At: now, Outcome: RoundUp})
	if err != nil || tr == nil || !tr.CameBack {
		t.Fatalf("coming back: %+v, %v", tr, err)
	}
	for i := 1; i <= websiteDownAfter; i++ {
		if _, err := s2.RecordWebsiteRound(ctx, id, websiteRoundResult{At: now.Add(time.Duration(i) * time.Minute), Outcome: "fail", Detail: "HTTP 500"}); err != nil {
			t.Fatal(err)
		}
	}
	outages, err = s2.WebsiteOutages(ctx, id, 10)
	if err != nil || len(outages) != 3 {
		t.Fatalf("after a new outage: %v, %v", outages, err)
	}
	if outages[0].ID <= outages[1].ID || outages[0].ID <= outages[2].ID {
		t.Errorf("the new outage reused an id: %+v", outages)
	}
}
