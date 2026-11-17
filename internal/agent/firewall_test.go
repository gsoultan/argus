package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

// fakeUFW is ufw in memory: numbered rules, active or not.
type fakeUFW struct {
	active bool
	rules  []string // as `ufw status numbered` prints them, without the number
	ran    []string
}

func (f *fakeUFW) run(_ context.Context, name string, args ...string) (string, error) {
	line := name + " " + strings.Join(args, " ")
	if name != "ufw" {
		return "", fmt.Errorf("unexpected %s", line)
	}
	if strings.Join(args, " ") == "status numbered" {
		st := "inactive"
		if f.active {
			st = "active"
		}
		out := "Status: " + st + "\n\n     To                         Action      From\n     --                         ------      ----\n"
		for i, r := range f.rules {
			out += fmt.Sprintf("[%2d] %s\n", i+1, r)
		}
		return out, nil
	}
	f.ran = append(f.ran, line)
	switch args[0] {
	case "--force":
		var n int
		fmt.Sscanf(args[2], "%d", &n)
		f.rules = slices.Delete(f.rules, n-1, n)
	case "allow", "deny":
		act := "ALLOW IN"
		if args[0] == "deny" {
			act = "DENY IN "
		}
		from, port, comment := "Anywhere", "", ""
		for i := 0; i < len(args)-1; i++ {
			switch args[i] {
			case "from":
				from = args[i+1]
			case "port":
				port = args[i+1]
			case "comment":
				comment = args[i+1]
			}
		}
		f.rules = append(f.rules, fmt.Sprintf("%-27s%s    %-27s# %s", port+"/tcp", act, from, comment))
	}
	return "", nil
}

func ufwOnly(t *testing.T) {
	old := lookPath
	lookPath = func(n string) bool { return n == "ufw" }
	t.Cleanup(func() { lookPath = old })
}

func TestUFWSelectedAddsAllowsThenDenyAndKeepsOthers(t *testing.T) {
	ufwOnly(t)
	f := &fakeUFW{active: true, rules: []string{
		"22/tcp                     ALLOW IN    Anywhere",
		"6379/tcp                   ALLOW IN    10.9.9.9                   # argus 6379",
	}}
	res := firewallDo(context.Background(), f.run, tunnel.FirewallRequest{Port: 6379, Reach: tunnel.ReachSelected,
		Sources: []string{"10.0.0.61", "10.0.0.50", "10.0.0.50"}})
	if res.Error != "" || res.Firewall != "ufw" || !res.Active {
		t.Fatalf("result %+v", res)
	}
	want := []string{
		"ufw --force delete 2",
		"ufw allow from 10.0.0.61 to any port 6379 proto tcp comment argus 6379",
		"ufw allow from 10.0.0.50 to any port 6379 proto tcp comment argus 6379",
		"ufw deny proto tcp to any port 6379 comment argus 6379",
	}
	if !slices.Equal(f.ran, want) {
		t.Fatalf("ran\n%s\nwant\n%s", strings.Join(f.ran, "\n"), strings.Join(want, "\n"))
	}
	if len(res.Mine) != 3 || len(res.Others) != 0 || res.OpenToAll || res.Mine[0] != "6379/tcp ALLOW IN 10.0.0.61 # argus 6379" {
		t.Fatalf("mine %v others %v open %v", res.Mine, res.Others, res.OpenToAll)
	}
	if !slices.Equal(res.Sources, []string{"10.0.0.61", "10.0.0.50"}) || res.AllowAll {
		t.Fatalf("sources %v allowAll %v", res.Sources, res.AllowAll)
	}
	if !strings.HasPrefix(f.rules[0], "22/tcp") {
		t.Fatalf("the SSH rule is gone: %v", f.rules)
	}
}

func TestUFWLocalRemovesOnlyArgusRules(t *testing.T) {
	ufwOnly(t)
	f := &fakeUFW{active: true, rules: []string{
		"6379/tcp                   ALLOW IN    10.0.0.50                  # argus 6379",
		"6379/tcp                   ALLOW IN    10.0.0.7                   # team cache",
		"6379/tcp                   DENY IN     Anywhere                   # argus 6379",
		"5432/tcp                   ALLOW IN    10.0.0.50                  # argus 5432",
	}}
	res := firewallDo(context.Background(), f.run, tunnel.FirewallRequest{Port: 6379, Reach: tunnel.ReachLocal})
	if res.Error != "" {
		t.Fatal(res.Error)
	}
	if !slices.Equal(f.ran, []string{"ufw --force delete 3", "ufw --force delete 1"}) {
		t.Fatalf("ran %v", f.ran)
	}
	if len(f.rules) != 2 || !strings.Contains(f.rules[0], "team cache") || !strings.Contains(f.rules[1], "argus 5432") {
		t.Fatalf("left %v", f.rules)
	}
}

func TestUFWOffIsLeftOff(t *testing.T) {
	ufwOnly(t)
	f := &fakeUFW{active: false}
	res := firewallDo(context.Background(), f.run, tunnel.FirewallRequest{Port: 6379, Reach: tunnel.ReachAny})
	if res.Active || len(f.ran) != 0 || res.Error != "" {
		t.Fatalf("an inactive ufw was touched: %+v ran %v", res, f.ran)
	}
}

func TestUFWReportsARuleThatAlreadyLetsEveryoneIn(t *testing.T) {
	ufwOnly(t)
	f := &fakeUFW{active: true, rules: []string{"6000:7000/tcp              ALLOW IN    Anywhere"}}
	res := firewallDo(context.Background(), f.run, tunnel.FirewallRequest{Port: 6379, Check: true})
	if !res.OpenToAll || len(f.ran) != 0 {
		t.Fatalf("%+v ran %v", res, f.ran)
	}
}

func TestPortIn(t *testing.T) {
	for to, want := range map[string]bool{
		"6379/tcp": true, "6379": true, "6379/udp": false, "6380/tcp": false, "6379,6380/tcp": true,
		"6000:7000/tcp": true, "Anywhere": true, "6379/tcp (v6)": true, "10.0.0.5 6379/tcp": true, "22/tcp": false,
	} {
		if got := portIn(to, 6379); got != want {
			t.Errorf("portIn(%q) = %v", to, got)
		}
	}
}

// fakeFirewalld keeps rich rules and ports, runtime only (permanent mirrors it).
type fakeFirewalld struct {
	running bool
	ports   []string
	rich    []string
	ran     []string
}

func (f *fakeFirewalld) run(_ context.Context, name string, args ...string) (string, error) {
	if name != "firewall-cmd" {
		return "", fmt.Errorf("unexpected %s", name)
	}
	a := strings.Join(args, " ")
	switch {
	case a == "--state":
		if f.running {
			return "running\n", nil
		}
		return "not running\n", fmt.Errorf("not running")
	case a == "--list-ports":
		return strings.Join(f.ports, " ") + "\n", nil
	case a == "--list-rich-rules":
		return strings.Join(f.rich, "\n") + "\n", nil
	}
	f.ran = append(f.ran, a)
	if args[0] == "--permanent" {
		return "success", nil
	}
	k, v, _ := strings.Cut(args[0], "=")
	switch k {
	case "--add-rich-rule":
		f.rich = append(f.rich, v)
	case "--remove-rich-rule":
		f.rich = slices.DeleteFunc(f.rich, func(x string) bool { return x == v })
	case "--add-port":
		f.ports = append(f.ports, v)
	case "--remove-port":
		f.ports = slices.DeleteFunc(f.ports, func(x string) bool { return x == v })
	}
	return "success", nil
}

func TestFirewalldRemembersItsOwnRules(t *testing.T) {
	old := lookPath
	lookPath = func(n string) bool { return n == "firewall-cmd" }
	t.Cleanup(func() { lookPath = old })
	state := filepath.Join(t.TempDir(), "fw.json")
	user := `rule family="ipv4" source address="10.0.0.7" port port="6379" protocol="tcp" accept`
	f := &fakeFirewalld{running: true, rich: []string{user}}

	res := firewallDo(context.Background(), f.run, tunnel.FirewallRequest{Port: 6379, Reach: tunnel.ReachSelected,
		Sources: []string{"10.0.0.50", "fd00::/64"}, StateFile: state})
	if res.Error != "" || len(res.Mine) != 2 || len(res.Others) != 1 {
		t.Fatalf("%+v", res)
	}
	if !slices.Equal(res.Sources, []string{"10.0.0.50", "fd00::/64"}) {
		t.Fatalf("sources %v", res.Sources)
	}
	if !strings.Contains(res.Mine[1], `family="ipv6" source address="fd00::/64"`) {
		t.Fatalf("v6 rule %q", res.Mine[1])
	}
	res = firewallDo(context.Background(), f.run, tunnel.FirewallRequest{Port: 6379, Reach: tunnel.ReachLocal, StateFile: state})
	if res.Error != "" || len(res.Mine) != 0 {
		t.Fatalf("%+v", res)
	}
	if !slices.Equal(f.rich, []string{user}) {
		t.Fatalf("the person's own rule went too: %v", f.rich)
	}
}

func TestFirewallRequestRefuses(t *testing.T) {
	for _, r := range []tunnel.FirewallRequest{
		{Port: 0, Reach: tunnel.ReachLocal},
		{Port: 6379, Reach: "open"},
		{Port: 6379, Reach: tunnel.ReachSelected},
		{Port: 6379, Reach: tunnel.ReachSelected, Sources: []string{"0.0.0.0/0"}},
		{Port: 6379, Reach: tunnel.ReachSelected, Sources: []string{"127.0.0.1"}},
		{Port: 6379, Reach: tunnel.ReachSelected, Sources: []string{"10.0.0.5; reboot"}},
		{Port: 6379, Reach: tunnel.ReachAny, Sources: []string{"10.0.0.5"}},
	} {
		if err := r.Validate(); err == nil {
			t.Errorf("%+v was accepted", r)
		}
	}
}
