package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

func TestRabbitConfBindAndSet(t *testing.T) {
	for conf, want := range map[string]string{
		"":                                       "*",
		"loopback_users.guest = true\n":          "*",
		"listeners.tcp.default = 5672\n":         "*",
		"listeners.tcp.local = 127.0.0.1:5672\n": "127.0.0.1",
		"listeners.tcp.1 = 127.0.0.1:5672\nlisteners.tcp.2 = 10.0.0.5:5672 # app\n": "127.0.0.1,10.0.0.5",
		"listeners.tcp.1 = 0.0.0.0:5672\n":                                          "*",
		"listeners.tcp = none\n":                                                    "",
	} {
		if got := rabbitConfBind([]byte(conf)); got != want {
			t.Errorf("%q reads as %q, want %q", conf, got, want)
		}
	}
	conf := "## RabbitMQ\nlisteners.tcp.default = 5672\nloopback_users.guest = true\n\n"
	b := setRabbitBind([]byte(conf), "127.0.0.1,10.0.0.183", 5672)
	want := "## RabbitMQ\nloopback_users.guest = true\n\n" + rabbitArgusHeader + "\nlisteners.tcp.1 = 127.0.0.1:5672\nlisteners.tcp.2 = 10.0.0.183:5672\n"
	if string(b) != want {
		t.Fatalf("got\n%s\nwant\n%s", b, want)
	}
	if rabbitConfBind(b) != "127.0.0.1,10.0.0.183" {
		t.Fatal("does not read back")
	}
	// Written again, Argus's lines replace Argus's lines.
	again := setRabbitBind(b, "*", 5672)
	if strings.Count(string(again), rabbitArgusHeader) != 1 || !strings.HasSuffix(string(again), "\nlisteners.tcp.default = 5672\n") ||
		!strings.HasPrefix(string(again), "## RabbitMQ\nloopback_users.guest = true\n") {
		t.Fatalf("again:\n%s", again)
	}
	// A new file.
	if got := string(setRabbitBind(nil, "127.0.0.1", 5672)); got != rabbitArgusHeader+"\nlisteners.tcp.1 = 127.0.0.1:5672\n" {
		t.Fatalf("new %q", got)
	}
}

// rabbitNetFake is a node as rabbitmqctl tells of it: it listens as its
// rabbitmq.conf said when it last started.
type rabbitNetFake struct {
	conf     string
	running  string
	guest    bool // the guest user signs in with guest
	loopback bool // and is kept to the loopback
	advanced string
}

func (f *rabbitNetFake) ctl(_ context.Context, args []string, _ string) ([]byte, error) {
	switch args[0] {
	case "status":
		var ls []string
		for _, a := range strings.Split(f.running, ",") {
			ip := a
			if a == "*" {
				ip = "::"
			}
			ls = append(ls, fmt.Sprintf(`{"interface":"[%s]","port":5672,"protocol":"amqp"}`, ip))
		}
		files := `["` + f.conf + `"]`
		if f.advanced != "" {
			files = `["` + f.conf + `","` + f.advanced + `"]`
		}
		return []byte(`{"pid":42,"rabbitmq_version":"3.13.7","config_files":` + files + `,"listeners":[{"interface":"[::]","port":25672,"protocol":"clustering"},` +
			strings.Join(ls, ",") + `]}`), nil
	case "authenticate_user":
		if f.guest {
			return []byte("Success"), nil
		}
		return nil, errors.New("authentication failed")
	case "eval":
		if f.loopback {
			return []byte(`{ok,[<<"guest">>]}`), nil
		}
		return []byte(`{ok,[]}`), nil
	case "list_connections":
		return []byte(`[{"peer_host":"127.0.0.1"},{"peer_host":"10.0.0.61"}]`), nil
	}
	return nil, errors.New("unexpected rabbitmqctl " + args[0])
}

func rabbitNetSession(t *testing.T, level string, fw *fakeRedisFirewall, log *[]string) (*rabbitSession, *rabbitNetFake, *stmtRecorder, string) {
	t.Helper()
	dir := t.TempDir()
	conf := filepath.Join(dir, "rabbitmq.conf")
	if err := os.WriteFile(conf, []byte("loopback_users.guest = true\nlisteners.tcp.local = 127.0.0.1:5672\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &rabbitNetFake{conf: conf, running: "127.0.0.1", loopback: true, guest: true}
	cfg := RabbitConfig{Ctl: fake.ctl, StoreConfig: StoreConfig{
		Firewall: func(ctx context.Context, r tunnel.FirewallRequest) tunnel.FirewallResult {
			if !r.Check {
				*log = append(*log, "firewall "+r.Reach)
			}
			return fw.do(ctx, r)
		},
		Restart: func(_ context.Context, unit string) error {
			b, _ := os.ReadFile(conf)
			fake.running = rabbitConfBind(b)
			*log = append(*log, "restart "+unit+" "+fake.running)
			return nil
		},
	}}
	f := &rabbitFactory{cfg: cfg, uses: &userUse{}, vhosts: map[string]map[string]int{}}
	rec, err := newStmtRecorder(filepath.Join(dir, "rec"), "s")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.f.Close() })
	s := &rabbitSession{f: f, g: tunnel.Grant{Host: "mq-01", Email: "ana@corp.example"}, db: tunnel.GrantDB{Name: "/", Level: level}}
	return s, fake, rec, filepath.Join(dir, "rec", "s.argussql")
}

func TestRabbitNetworkSelectedNarrowsTheFirewallFirstAndRestarts(t *testing.T) {
	oldAddrs, oldSvc := hostAddresses, serviceOf
	hostAddresses = func() []RedisAddress { return nil }
	serviceOf = func(int, ...string) string { return "rabbitmq-server.service" }
	t.Cleanup(func() { hostAddresses, serviceOf = oldAddrs, oldSvc })

	var log []string
	s, fake, rec, recPath := rabbitNetSession(t, tunnel.LevelDBA, &fakeRedisFirewall{active: true}, &log)
	ctx := context.Background()
	if r := s.ready(); !slices.Contains(r.Ops, tunnel.RabbitOpNetworkSet) {
		t.Fatalf("ready ops %v", r.Ops)
	}
	a, err := s.do(ctx, tunnel.StoreRequest{ID: 1, Op: tunnel.RabbitOpNetwork}, rec)
	if err != nil || a.Type != "result" {
		t.Fatalf("view: %v %+v", err, a)
	}
	if n := a.Data.(RabbitNetwork); n.Reach != tunnel.ReachLocal || n.Clients != 2 || n.Remote != 1 || n.GuestRemote || n.Version != "3.13.7" {
		t.Fatalf("view %+v", n)
	}
	req := tunnel.StoreRequest{ID: 2, Op: tunnel.RabbitOpNetworkSet, Reach: tunnel.ReachSelected,
		Addresses: []string{"10.0.0.183"}, Sources: []string{"10.0.0.61"}, Restart: true}
	a, err = s.do(ctx, req, rec)
	if err != nil || a.Type != "result" {
		t.Fatalf("set: %v %+v", err, a)
	}
	if !slices.Equal(log, []string{"firewall selected", "restart rabbitmq-server.service 127.0.0.1,10.0.0.183"}) {
		t.Fatalf("ran %v", log)
	}
	if res := a.Data.(RabbitNetworkResult); !res.Restarted || res.Network.Reach != tunnel.ReachSelected || res.Network.PendingRestart {
		t.Fatalf("result %+v", res)
	}
	b, _ := os.ReadFile(fake.conf)
	if !strings.HasPrefix(string(b), "loopback_users.guest = true\n\n"+rabbitArgusHeader) {
		t.Fatalf("the file:\n%s", b)
	}
	recs := mongoRecords(t, recPath)
	if len(recs) != 2 || recs[0].Kind != "stmt" || recs[0].SQL != tunnel.RabbitNetworkText(req) || recs[1].Kind != "done" {
		t.Fatalf("records %+v", recs)
	}

	// Closing stops the listener before the firewall's rules go.
	log = nil
	a, _ = s.do(ctx, tunnel.StoreRequest{ID: 3, Op: tunnel.RabbitOpNetworkSet, Reach: tunnel.ReachLocal, Restart: true}, rec)
	if a.Type != "result" || !slices.Equal(log, []string{"restart rabbitmq-server.service 127.0.0.1", "firewall local"}) {
		t.Fatalf("close: %v %+v", log, a)
	}
}

func TestRabbitNetworkRefusals(t *testing.T) {
	oldAddrs := hostAddresses
	hostAddresses = func() []RedisAddress { return nil }
	t.Cleanup(func() { hostAddresses = oldAddrs })
	ctx := context.Background()
	var log []string
	sel := tunnel.StoreRequest{ID: 1, Op: tunnel.RabbitOpNetworkSet, Reach: tunnel.ReachNetwork,
		Addresses: []string{"10.0.0.183"}, Sources: []string{"10.0.0.0/24"}}
	refused := func(s *rabbitSession, rec *stmtRecorder, req tunnel.StoreRequest, want string) tunnel.StoreAnswer {
		t.Helper()
		a, err := s.do(ctx, req, rec)
		if err != nil || a.Type != "error" || !strings.Contains(a.Error, want) {
			t.Fatalf("want a refusal with %q, got %v %+v", want, err, a)
		}
		return a
	}
	s, _, rec, _ := rabbitNetSession(t, tunnel.LevelOwner, &fakeRedisFirewall{active: true}, &log)
	refused(s, rec, tunnel.StoreRequest{Op: tunnel.RabbitOpNetwork}, "DBA work")
	// guest/guest, not kept to the loopback.
	s, fake, rec, recPath := rabbitNetSession(t, tunnel.LevelDBA, &fakeRedisFirewall{active: true}, &log)
	fake.loopback = false
	refused(s, rec, sel, "guest user")
	// advanced.config sets the listeners.
	s, fake, rec, _ = rabbitNetSession(t, tunnel.LevelDBA, &fakeRedisFirewall{active: true}, &log)
	adv := filepath.Join(filepath.Dir(fake.conf), "advanced.config")
	_ = os.WriteFile(adv, []byte("[{rabbit, [{tcp_listeners, [5672]}]}]."), 0o644)
	fake.advanced = adv
	refused(s, rec, sel, "advanced.config")
	// Anywhere waits for approval, then needs the name.
	s, _, rec, _ = rabbitNetSession(t, tunnel.LevelDBA, &fakeRedisFirewall{active: true}, &log)
	anyReq := tunnel.StoreRequest{ID: 4, Op: tunnel.RabbitOpNetworkSet, Reach: tunnel.ReachAny}
	if a := refused(s, rec, anyReq, "approves"); a.NeedsApproval == nil || a.NeedsApproval.Text != tunnel.RabbitNetworkText(anyReq) {
		t.Fatalf("needs approval %+v", a.NeedsApproval)
	}
	anyReq.Approval = "11111111-1111-1111-1111-111111111111"
	refused(s, rec, anyReq, "type the server's name")
	if len(log) != 0 {
		t.Fatalf("a refusal ran %v", log)
	}
	for _, r := range mongoRecords(t, recPath) {
		if r.Kind != "error" {
			t.Fatalf("a refusal recorded a change: %+v", r)
		}
	}
}
