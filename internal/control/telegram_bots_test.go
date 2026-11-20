package control

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/notify"
	"github.com/gsoultan/argus/internal/tunnel"
)

// fakeTelegram is a Bot API for several bots: it records which bot posted
// what to which chat, answers getMe for the tokens it knows, and lists chats
// for getUpdates.
type fakeTelegram struct {
	mu   sync.Mutex
	sent map[string][]string // "token|chat" -> texts
	srv  *httptest.Server
}

func newFakeTelegram(t *testing.T) *fakeTelegram {
	f := &fakeTelegram{sent: map[string][]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		// /bot<token>/<method>
		rest := strings.TrimPrefix(r.URL.Path, "/bot")
		token, method, _ := strings.Cut(rest, "/")
		switch method {
		case "getMe":
			if strings.HasSuffix(token, "bad") {
				_, _ = w.Write([]byte(`{"ok":false,"description":"Unauthorized"}`))
				return
			}
			name := "bot_" + token[:3]
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"username":"` + name + `","first_name":"B"}}`))
		case "getUpdates":
			_, _ = w.Write([]byte(`{"ok":true,"result":[
				{"my_chat_member":{"chat":{"id":-1001,"type":"supergroup","title":"Payments"}}},
				{"message":{"chat":{"id":77,"type":"private","first_name":"Dewi"}}},
				{"message":{"chat":{"id":-1001,"type":"supergroup","title":"Payments"}}}]}`))
		case "sendMessage":
			f.mu.Lock()
			k := token + "|" + r.Form.Get("chat_id")
			f.sent[k] = append(f.sent[k], r.Form.Get("text"))
			f.mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTelegram) to(token, chat string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent[token+"|"+chat]...)
}

func (f *fakeTelegram) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, v := range f.sent {
		n += len(v)
	}
	return n
}

func (f *fakeTelegram) wait(t *testing.T, token, chat string) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(f.to(token, chat)) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	return f.to(token, chat)
}

const (
	tokOps = "111111:AAopsopsopsopsopsopsopsopsops"
	tokPay = "222222:AApaypaypaypaypaypaypaypaypay"
	tokPlt = "333333:AAplatformplatformplatformplat"
)

func telegramTestAPI(t *testing.T) (*API, *Store, *fakeTelegram) {
	t.Helper()
	s := testStore(t)
	api := NewAPI(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	key, err := tunnel.GrantKeyFromSecret("a-test-signing-secret-that-is-long-enough-0123")
	if err != nil {
		t.Fatal(err)
	}
	api.Tunnels = &TunnelHub{GrantKey: key}
	f := newFakeTelegram(t)
	api.telegramAPI = f.srv.URL
	plat, err := notify.NewTelegram(notify.TelegramConfig{BotToken: tokPlt, ChatIDs: []string{"-100plat"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	api.SignInAlerts = notify.NewSignInNotifier(nil, plat.WithAPI(f.srv.URL), "https://argus.example.com", nil)
	t.Cleanup(func() { api.SignInAlerts.Close(context.Background()) })
	clean := func() {
		ctx := context.Background()
		_, _ = s.pool.Exec(ctx, `DELETE FROM telegram_bots`)
		_, _ = s.pool.Exec(ctx, `DELETE FROM telegram_routes`)
	}
	clean()
	t.Cleanup(clean)
	return api, s, f
}

// Bots are kept per organization with the token sealed, each kind of alert
// goes through the bot it chose and only to that bot's chats, and the
// default organization keeps the platform bot until it has a default of
// its own.
func TestTelegramBotsAndRoutes(t *testing.T) {
	api, s, f := telegramTestAPI(t)
	ctx := context.Background() // the default organization

	// No bots yet: the default organization still uses the platform bot.
	if tg, why := api.telegramForKind(ctx, TelegramRouteSignIn); tg.bot == nil || tg.name != "Platform bot" {
		t.Fatalf("before any bot: %+v, %s", tg, why)
	}
	// Backups never go through the platform bot here: its channels have them.
	if tg, _ := api.telegramForKind(ctx, TelegramRouteBackup); tg.bot != nil {
		t.Fatalf("backups went to %s", tg.name)
	}

	add := func(name, token string, chats []notify.TelegramChat, def bool) TelegramBot {
		t.Helper()
		sealed, err := api.sealTelegramToken(ctx, token)
		if err != nil {
			t.Fatal(err)
		}
		b, err := s.SaveTelegramBot(ctx, "", TelegramBotInput{Name: name, Chats: chats, Default: def}, "u", tokenTail(token), sealed, "admin@northwind.id")
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	ops := add("Ops", tokOps, []notify.TelegramChat{{ID: "-100ops", Title: "Ops"}, {ID: "-100infra", Title: "Infra"}}, true)
	pay := add("Payments", tokPay, []notify.TelegramChat{{ID: "-100pay", Title: "Payments"}}, false)

	// The token is sealed, not stored as it was.
	var raw []byte
	if err := s.pool.QueryRow(ctx, `SELECT token_sealed FROM telegram_bots WHERE id = $1`, ops.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(tokOps)) || ops.TokenTail != "ops" {
		t.Fatalf("token kept in the clear, or tail %q", ops.TokenTail)
	}

	// The default bot now takes "default"; a kind can pick another bot and
	// some of its chats, and a chat it does not have is never posted to.
	if tg, _ := api.telegramForKind(ctx, TelegramRouteSignIn); tg.botID != ops.ID || strings.Join(tg.chats, ",") != "-100ops,-100infra" {
		t.Fatalf("default bot: %+v", tg)
	}
	if err := s.SetTelegramRoute(ctx, TelegramRoute{Kind: TelegramRouteServer, Bot: ops.ID, Chats: []string{"-100infra", "-100elsewhere"}}, "a"); err != nil {
		t.Fatal(err)
	}
	if tg, _ := api.telegramForKind(ctx, TelegramRouteServer); strings.Join(tg.chats, ",") != "-100infra" {
		t.Fatalf("server chats: %v", tg.chats)
	}
	if err := s.SetTelegramRoute(ctx, TelegramRoute{Kind: TelegramRouteWebsite, Bot: BotOff}, "a"); err != nil {
		t.Fatal(err)
	}

	// A website on its own bot sends through it, even with the Websites
	// choice off; one following the choice sends nothing.
	mk := func(in WebsiteInput) WebsiteCheck {
		in.URL = "https://" + unique("site") + ".example.com/"
		c, err := s.CreateWebsiteCheck(ctx, in, "admin@northwind.id")
		if err != nil {
			t.Fatalf("CreateWebsiteCheck: %v", err)
		}
		t.Cleanup(func() { _, _ = s.DeleteWebsiteCheck(context.Background(), c.ID) })
		return c
	}
	own := mk(WebsiteInput{Telegram: TelegramChats, TelegramBot: pay.ID, TelegramChats: []string{"-100pay"}})
	follows := mk(WebsiteInput{})
	if tg, why := api.websiteTelegram(ctx, follows); tg.bot != nil || why != "Telegram is off for all websites" {
		t.Fatalf("following an Off choice: %+v %q", tg, why)
	}
	api.announceWebsite(ctx, websiteTransition{WentDown: true, Check: own,
		Outage: WebsiteOutage{StartedAt: time.Now().Add(-2 * time.Minute), Cause: "answered 502 Bad Gateway"}})
	if got := f.wait(t, tokPay, "-100pay"); len(got) != 1 || !strings.Contains(got[0], "is down") {
		t.Fatalf("the payments chat got %q", got)
	}
	if f.total() != 1 {
		t.Errorf("other chats were posted to: %v", f.sent)
	}
	// The send is recorded on the bot.
	deadline := time.Now().Add(3 * time.Second)
	for {
		b, _, _ := s.TelegramBot(ctx, pay.ID)
		if b.LastOKAt != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the bot's last message was not recorded")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The view says what uses each bot.
	v, err := api.telegramView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	used := map[string]string{}
	for _, b := range v.Bots {
		used[b.Name] = strings.Join(b.UsedBy, "; ")
	}
	if used["Ops"] != "Sign-in alerts; Server connected; Backups" || !strings.Contains(used["Payments"], "Website ") {
		t.Errorf("used by: %v", used)
	}

	// Removing a bot moves what used it to the choice made.
	if err := s.DeleteTelegramBot(ctx, pay.ID, BotOff); err != nil {
		t.Fatal(err)
	}
	c, err := s.WebsiteCheck(ctx, own.ID)
	if err != nil || c.Telegram != TelegramOff || c.TelegramBot != "" {
		t.Errorf("after removing its bot the site is %q/%q (%v)", c.Telegram, c.TelegramBot, err)
	}

	// Another organization neither sees these bots nor uses the platform's.
	tn, _, _ := newTenant(t, s)
	other := WithTenant(ctx, tn)
	if bots, err := s.TelegramBots(other); err != nil || len(bots) != 0 {
		t.Errorf("another organization sees %d bots (%v)", len(bots), err)
	}
	if tg, why := api.telegramForKind(other, TelegramRouteSignIn); tg.bot != nil || why == "" {
		t.Errorf("another organization would post through %s", tg.name)
	}
}

// The routes: admins only, a token checked with Telegram before it is kept
// and never returned, chats listed from the bot's updates, and changes in
// the audit log.
func TestTelegramRoutesHTTP(t *testing.T) {
	h := newTenantHarness(t)
	key, err := tunnel.GrantKeyFromSecret("a-test-signing-secret-that-is-long-enough-0123")
	if err != nil {
		t.Fatal(err)
	}
	h.api.Tunnels = &TunnelHub{GrantKey: key}
	f := newFakeTelegram(t)
	h.api.telegramAPI = f.srv.URL
	tn, owner, pw := newTenant(t, h.store)
	c := h.signIn(t, tn.Slug, owner, pw)
	ctx := WithTenant(context.Background(), tn)
	call := func(cl *http.Client, method, path string, body any) (int, map[string]any) {
		t.Helper()
		b := ""
		if body != nil {
			raw, _ := json.Marshal(body)
			b = string(raw)
		}
		code, text := h.call(t, cl, "", method, path, b)
		out := map[string]any{}
		_ = json.Unmarshal([]byte(text), &out)
		return code, out
	}

	opEmail := unique("op") + "@northwind.id"
	if _, err := h.store.CreateAccount(ctx, opEmail, "Op", "operator", "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	op := h.signIn(t, tn.Slug, opEmail, "correct horse battery staple")
	if code, _ := call(op, "GET", "/api/v1/telegram", nil); code != http.StatusForbidden {
		t.Errorf("an operator reading bots = %d, want 403", code)
	}

	if code, out := call(c, "POST", "/api/v1/telegram/bots", map[string]any{"name": "Broken", "token": "999999:AAbadbadbadbadbadbadbadbadbad"}); code != http.StatusBadRequest {
		t.Errorf("a refused token = %d %v, want 400", code, out)
	}
	code, out := call(c, "POST", "/api/v1/telegram/chats", map[string]any{"token": tokPay})
	if chats, _ := out["chats"].([]any); code != http.StatusOK || len(chats) != 2 {
		t.Fatalf("chats = %d %v", code, out)
	}
	code, out = call(c, "POST", "/api/v1/telegram/bots", map[string]any{"name": "Payments", "token": tokPay,
		"chats": []map[string]string{{"id": "-1001", "title": "Payments"}}, "default": true})
	if code != http.StatusCreated || out["username"] != "bot_222" || out["tokenTail"] != "pay" {
		t.Fatalf("add = %d %v", code, out)
	}
	if raw, _ := json.Marshal(out); bytes.Contains(raw, []byte(tokPay)) {
		t.Fatal("the token came back out")
	}
	id, _ := out["id"].(string)
	if code, out := call(c, "POST", "/api/v1/telegram/bots/"+id+"/test", nil); code != http.StatusOK {
		t.Fatalf("test = %d %v", code, out)
	}
	if got := f.to(tokPay, "-1001"); len(got) != 1 {
		t.Fatalf("test message: %v", got)
	}
	if code, out := call(c, "PUT", "/api/v1/telegram/routes/signin", map[string]any{"bot": "nope"}); code != http.StatusBadRequest {
		t.Errorf("a route to no bot = %d %v", code, out)
	}
	if code, out := call(c, "PUT", "/api/v1/telegram/routes/backup", map[string]any{"bot": "platform"}); code != http.StatusBadRequest {
		t.Errorf("backups to the platform bot = %d %v", code, out)
	}
	if code, out := call(c, "PUT", "/api/v1/telegram/routes/server", map[string]any{"bot": id, "chats": []string{"-1001"}}); code != http.StatusOK {
		t.Errorf("route = %d %v", code, out)
	}
	if code, out := call(c, "POST", "/api/v1/telegram/routes/server/test", nil); code != http.StatusOK {
		t.Errorf("route test = %d %v", code, out)
	}
	code, out = call(c, "GET", "/api/v1/telegram", nil)
	if code != http.StatusOK || out["platform"] != nil {
		t.Errorf("view = %d, platform %v (another organization must not see it)", code, out["platform"])
	}
	if code, _ := call(c, "DELETE", "/api/v1/telegram/bots/"+id+"?moveTo=off", nil); code != http.StatusNoContent {
		t.Errorf("remove = %d", code)
	}

	evs, err := h.store.AuditEvents(ctx, 30, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, ev := range evs {
		if strings.HasPrefix(ev.Action, "telegram.") {
			seen[ev.Action] = true
			if strings.Contains(ev.Detail, tokPay) {
				t.Errorf("%s put the token in the audit log", ev.Action)
			}
		}
	}
	for _, a := range []string{"telegram.bot.add", "telegram.route", "telegram.bot.remove"} {
		if !seen[a] {
			t.Errorf("no %s in the audit log", a)
		}
	}
}
