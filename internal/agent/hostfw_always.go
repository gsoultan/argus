package agent

import (
	"bufio"
	"context"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
What stays open going out when a firewall policy blocks everything else
(plans/server-firewall/design.md, "Always allowed"):

  - the agent's own connection to Argus, which is also where agent updates
    come from: every port to the control plane's addresses;
  - name lookups: port 53 to the resolvers the server is set to use, or to
    everyone when none can be read;
  - operating system updates: web traffic to the package sources the
    server reads (apt, dnf, yum, zypper). On Windows, Windows Update's own
    service, by program, since its servers are too many to name.

Firewalls match addresses, not names, so each change looks the names up
again; the control plane sends a change at least every hour to a server
that blocks going out, which keeps them current.
*/

// FirewallAlways builds the lines that always stay open going out.
func FirewallAlways(ctrl func() []netip.Addr) func() ([]tunnel.FirewallLine, error) {
	return func() ([]tunnel.FirewallLine, error) {
		var addrs []netip.Addr
		if ctrl != nil {
			addrs = ctrl()
		}
		if len(addrs) == 0 {
			return nil, errNoControlAddr
		}
		ctl := tunnel.FirewallLine{Key: "awctl", Direction: "out", Action: "allow", Protocol: "any"}
		for _, a := range addrs {
			ctl.Sources = append(ctl.Sources, a.String())
		}
		dns := resolvers()
		lines := []tunnel.FirewallLine{ctl,
			{Key: "awdns", Direction: "out", Action: "allow", Protocol: "udp", Ports: "53", Sources: dns},
			{Key: "awdnt", Direction: "out", Action: "allow", Protocol: "tcp", Ports: "53", Sources: dns},
		}
		if runtime.GOOS == "windows" {
			lines = append(lines, tunnel.FirewallLine{Key: "awupd", Direction: "out", Action: "allow", Protocol: "any"})
		} else if ips := packageSourceAddrs(); len(ips) > 0 {
			lines = append(lines, tunnel.FirewallLine{Key: "awupd", Direction: "out", Action: "allow", Protocol: "tcp",
				Ports: "80,443", Sources: ips})
		}
		for i := range lines {
			if err := lines[i].Validate(); err != nil {
				return nil, err
			}
		}
		return lines, nil
	}
}

// resolvers are the name servers the server uses, leaving out the local
// stub (systemd-resolved's 127.0.0.53), which the loopback rule covers;
// systemd-resolved's own upstreams are read instead.
func resolvers() []string {
	var out []string
	for _, p := range []string{"/etc/resolv.conf", "/run/systemd/resolve/resolv.conf"} {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fs := strings.Fields(sc.Text())
			if len(fs) < 2 || fs[0] != "nameserver" {
				continue
			}
			a, err := netip.ParseAddr(strings.Split(fs[1], "%")[0])
			if err != nil || a.IsLoopback() || a.IsUnspecified() {
				continue
			}
			if s := a.Unmap().String(); !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
		_ = f.Close()
	}
	return out
}

// packageSourceHosts are the hosts in the server's package sources.
func packageSourceHosts() []string {
	var hosts []string
	add := func(raw string) {
		raw = strings.Trim(strings.TrimSpace(raw), `"'`)
		if i := strings.IndexAny(raw, " \t"); i > 0 {
			raw = raw[:i]
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
			return
		}
		if h := u.Hostname(); !slices.Contains(hosts, h) && len(hosts) < 32 {
			hosts = append(hosts, h)
		}
	}
	read := func(glob string, each func(line string)) {
		paths, _ := filepath.Glob(glob)
		for _, p := range paths {
			b, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			for _, l := range strings.Split(string(b), "\n") {
				if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
					each(l)
				}
			}
		}
	}
	deb := func(l string) {
		f := strings.Fields(l)
		if len(f) >= 2 && (f[0] == "deb" || f[0] == "deb-src") {
			for _, x := range f[1:] {
				if strings.HasPrefix(x, "http") {
					add(x)
					return
				}
			}
		}
		if rest, ok := strings.CutPrefix(l, "URIs:"); ok {
			for _, x := range strings.Fields(rest) {
				add(x)
			}
		}
	}
	read("/etc/apt/sources.list", deb)
	read("/etc/apt/sources.list.d/*.list", deb)
	read("/etc/apt/sources.list.d/*.sources", deb)
	rpm := func(l string) {
		for _, k := range []string{"baseurl=", "mirrorlist=", "metalink="} {
			if rest, ok := strings.CutPrefix(l, k); ok {
				add(rest)
			}
		}
	}
	read("/etc/yum.repos.d/*.repo", rpm)
	read("/etc/zypp/repos.d/*.repo", rpm)
	return hosts
}

// packageSourceAddrs looks the package sources' hosts up.
func packageSourceAddrs() []string {
	var out []string
	for _, h := range packageSourceHosts() {
		if strings.Contains(h, "$") {
			continue // a dnf variable the agent cannot fill in
		}
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", h)
		cancel()
		if err != nil {
			continue
		}
		for _, ip := range ips {
			if s := ip.Unmap().String(); !slices.Contains(out, s) && len(out) < 200 {
				out = append(out, s)
			}
		}
	}
	slices.Sort(out)
	return out
}
