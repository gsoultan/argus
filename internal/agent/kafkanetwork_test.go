package agent

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

func TestKafkaListenerFor(t *testing.T) {
	for _, c := range []struct {
		listeners   string
		proto, bind string
		ok          bool
	}{
		{"SASL_PLAINTEXT://:9092,CONTROLLER://:9093", "SASL_PLAINTEXT", "*", true},
		{"SASL_SSL://0.0.0.0:9092", "SASL_SSL", "*", true},
		{"PLAINTEXT://127.0.0.1:9094,SASL_PLAINTEXT://localhost:9092", "SASL_PLAINTEXT", "127.0.0.1", true},
		{"SASL_PLAINTEXT://10.0.0.5:9092", "SASL_PLAINTEXT", "10.0.0.5", true},
		{"SASL_PLAINTEXT://[::]:9092", "SASL_PLAINTEXT", "*", true},
		{"PLAINTEXT://:9094", "", "", false},
	} {
		p, b, ok := kafkaListenerFor(c.listeners, 9092)
		if p != c.proto || b != c.bind || ok != c.ok {
			t.Errorf("%s: %q %q %v", c.listeners, p, b, ok)
		}
	}
}

func kafkaNetSession(t *testing.T, listeners string, fw *fakeRedisFirewall, log *[]string) (*kafkaSession, *stmtRecorder) {
	t.Helper()
	cfg := StoreConfig{Firewall: func(ctx context.Context, r tunnel.FirewallRequest) tunnel.FirewallResult {
		if !r.Check {
			*log = append(*log, "firewall "+r.Reach)
		}
		return fw.do(ctx, r)
	}}
	f := &kafkaFactory{cfg: cfg, uses: &userUse{}, netState: func(context.Context) (KafkaNetwork, error) {
		return KafkaNetwork{Listeners: listeners, Advertised: "SASL_PLAINTEXT://kafka-01:9092", Port: 9092, BrokerID: 1, Brokers: 1, Host: "kafka-01"}, nil
	}}
	rec, err := newStmtRecorder(filepath.Join(t.TempDir(), "rec"), "s")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.f.Close() })
	return &kafkaSession{f: f, g: tunnel.Grant{Host: "kafka-01"}, db: tunnel.GrantDB{Name: "*", Level: tunnel.LevelDBA}, ops: map[string]bool{}}, rec
}

func TestKafkaNetworkChangesTheFirewallOnly(t *testing.T) {
	oldAddrs := hostAddresses
	hostAddresses = func() []RedisAddress { return nil }
	t.Cleanup(func() { hostAddresses = oldAddrs })
	ctx := context.Background()
	var log []string
	fw := &fakeRedisFirewall{active: true}
	s, rec := kafkaNetSession(t, "SASL_PLAINTEXT://:9092,CONTROLLER://:9093", fw, &log)
	if r := s.ready(); !slices.Contains(r.Ops, tunnel.RabbitOpNetworkSet) {
		t.Fatalf("ops %v", r.Ops)
	}
	a, _ := s.do(ctx, tunnel.StoreRequest{Op: tunnel.RabbitOpNetwork}, rec)
	if n := a.Data.(KafkaNetwork); n.Reach != tunnel.ReachAny || n.Bind != "*" || n.Protocol != "SASL_PLAINTEXT" {
		t.Fatalf("view %+v", a)
	}
	req := tunnel.StoreRequest{ID: 2, Op: tunnel.RabbitOpNetworkSet, Reach: tunnel.ReachSelected,
		Addresses: []string{"10.0.0.183"}, Sources: []string{"10.0.0.61"}}
	a, _ = s.do(ctx, req, rec)
	if a.Type != "result" || !slices.Equal(log, []string{"firewall selected"}) || a.Data.(KafkaNetworkResult).Network.Reach != tunnel.ReachSelected {
		t.Fatalf("selected: %v %+v", log, a)
	}
	// Keeping it to this server would need its listeners changed.
	a, _ = s.do(ctx, tunnel.StoreRequest{ID: 3, Op: tunnel.RabbitOpNetworkSet, Reach: tunnel.ReachLocal}, rec)
	if a.Type != "error" || !strings.Contains(a.Error, "does not change Kafka's listeners") {
		t.Fatalf("local: %+v", a)
	}
	// A broker on the loopback only cannot be opened from here.
	s, rec = kafkaNetSession(t, "SASL_PLAINTEXT://127.0.0.1:9092", fw, &log)
	log = nil
	a, _ = s.do(ctx, req, rec)
	if a.Type != "error" || !strings.Contains(a.Error, "server.properties") || len(log) != 0 {
		t.Fatalf("loopback: %v %+v", log, a)
	}
	// Anywhere waits for approval.
	a, _ = s.do(ctx, tunnel.StoreRequest{ID: 4, Op: tunnel.RabbitOpNetworkSet, Reach: tunnel.ReachAny}, rec)
	if a.NeedsApproval == nil || a.NeedsApproval.Text != "FIREWALL ALLOW ANY" {
		t.Fatalf("anywhere: %+v", a)
	}
}
