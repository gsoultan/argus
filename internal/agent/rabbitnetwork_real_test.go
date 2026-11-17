//go:build !windows

package agent

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

// TestRabbitNetworkOnARealNode runs against the RabbitMQ on this machine,
// with ARGUS_TEST_RABBITMQ=1: it writes /etc/rabbitmq/rabbitmq.conf and
// restarts the node, so it is for a throwaway machine only.
func TestRabbitNetworkOnARealNode(t *testing.T) {
	if os.Getenv("ARGUS_TEST_RABBITMQ") != "1" {
		t.Skip("set ARGUS_TEST_RABBITMQ=1 on a throwaway machine with RabbitMQ")
	}
	ip := lanIP()
	if ip == "" {
		t.Skip("this machine has no address besides the loopback")
	}
	oldAddrs, oldSvc := hostAddresses, serviceOf
	hostAddresses = func() []RedisAddress { return []RedisAddress{{IP: ip, Iface: "eth0", Prefix: ip + "/24"}} }
	serviceOf = func(int, ...string) string { return "rabbitmq-server.service" }
	t.Cleanup(func() { hostAddresses, serviceOf = oldAddrs, oldSvc })
	if _, err := os.Stat(rabbitConfDefault); err == nil {
		t.Skip(rabbitConfDefault + " exists; this test writes it")
	}
	t.Cleanup(func() { _ = os.Remove(rabbitConfDefault) })

	ctx := context.Background()
	restart := func(ctx context.Context, _ string) error {
		if _, err := runRabbitmqctl(ctx, []string{"stop"}, ""); err != nil {
			return err
		}
		if err := exec.Command("rabbitmq-server", "-detached").Run(); err != nil {
			return err
		}
		for i := 0; i < 60; i++ {
			if _, err := runRabbitmqctl(ctx, []string{"await_startup"}, ""); err == nil {
				return nil
			}
			time.Sleep(time.Second)
		}
		return errors.New("the node did not start again")
	}
	fw := &fakeRedisFirewall{active: true}
	cfg := RabbitConfig{Ctl: runRabbitmqctl, StoreConfig: StoreConfig{Firewall: fw.do, Restart: restart}}
	f := &rabbitFactory{cfg: cfg, uses: &userUse{}, vhosts: map[string]map[string]int{}}
	rec, err := newStmtRecorder(t.TempDir(), "s")
	if err != nil {
		t.Fatal(err)
	}
	s := &rabbitSession{f: f, g: tunnel.Grant{Host: "mq-01"}, db: tunnel.GrantDB{Name: "/", Level: tunnel.LevelDBA}}

	a, _ := s.do(ctx, tunnel.StoreRequest{ID: 1, Op: tunnel.RabbitOpNetwork}, rec)
	n, ok := a.Data.(RabbitNetwork)
	if !ok || n.Port != 5672 || n.ConfigFile != rabbitConfDefault || n.GuestRemote || n.Version == "" {
		t.Fatalf("view %+v %+v", n, a)
	}
	a, _ = s.do(ctx, tunnel.StoreRequest{ID: 2, Op: tunnel.RabbitOpNetworkSet, Reach: tunnel.ReachSelected,
		Addresses: []string{ip}, Sources: []string{"10.0.0.61"}, Restart: true}, rec)
	res, ok := a.Data.(RabbitNetworkResult)
	if !ok || !res.Restarted || res.Network.Bind != "127.0.0.1,"+ip || res.Network.PendingRestart || res.Network.Reach != tunnel.ReachSelected {
		t.Fatalf("selected %+v %+v", res, a)
	}
	c, err := net.DialTimeout("tcp", net.JoinHostPort(ip, "5672"), 3*time.Second)
	if err != nil {
		t.Fatalf("not listening on %s: %v", ip, err)
	}
	c.Close()

	a, _ = s.do(ctx, tunnel.StoreRequest{ID: 3, Op: tunnel.RabbitOpNetworkSet, Reach: tunnel.ReachLocal, Restart: true}, rec)
	res, ok = a.Data.(RabbitNetworkResult)
	if !ok || res.Network.Bind != "127.0.0.1" || res.Network.Reach != tunnel.ReachLocal {
		t.Fatalf("local %+v %+v", res, a)
	}
	if c, err := net.DialTimeout("tcp", net.JoinHostPort(ip, "5672"), 3*time.Second); err == nil {
		c.Close()
		t.Fatalf("still listening on %s", ip)
	}
	b, _ := os.ReadFile(rabbitConfDefault)
	if !strings.Contains(string(b), "listeners.tcp.1 = 127.0.0.1:5672") {
		t.Fatalf("the file:\n%s", b)
	}
}
