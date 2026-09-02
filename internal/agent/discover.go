package agent

import (
	"bufio"
	"net"
	"os"
	"strconv"
	"strings"
)

// Facts is what a host reports about itself.
//
// The inventory is hand-written, which makes "Argus covers all privileged
// access" a claim rather than something anyone can check. A host that nobody
// remembered to add is exactly the one that gets compromised, so the agent
// reports what it is and lets the control plane compare that against what the
// inventory says exists.
//
// Nothing here is a secret. Account names, listening ports and addresses are
// visible to any local user; /etc/shadow is never read. The point is coverage,
// not reconnaissance, and an agent that collected more than it needed would be
// a liability on every host it runs on.
type Facts struct {
	Hostname string `json:"hostname"`
	// FQDN is the name the host answers to on the network, which is often what
	// the inventory records even when `hostname` returns the short form.
	FQDN string `json:"fqdn,omitempty"`
	// MachineID is a stable identity that survives a rename. Hostnames change;
	// an asset that appears twice under two names is a coverage gap wearing a
	// disguise.
	MachineID string `json:"machine_id,omitempty"`
	OS        string `json:"os,omitempty"`

	// Addresses are the non-loopback addresses this host answers on, so the
	// control plane can tell whether an inventory entry already points here.
	Addresses []string `json:"addresses,omitempty"`
	// SSHPorts are the ports sshd is configured to listen on. More than one is
	// worth surfacing: an inventory that knows about 22 and not 2222 leaves a
	// documented, unmonitored way in.
	SSHPorts []int `json:"ssh_ports,omitempty"`
	// Accounts are local accounts that can hold an interactive session, which
	// is the candidate list for principals on this host.
	Accounts []string `json:"accounts,omitempty"`
}

// DiscoverConfig points the scan at the files it reads.
type DiscoverConfig struct {
	OSRelease  string
	Passwd     string
	Shells     string
	MachineID  string
	SSHDConfig string
}

// DefaultDiscoverConfig reads the usual locations.
func DefaultDiscoverConfig() DiscoverConfig {
	return DiscoverConfig{
		OSRelease:  "/etc/os-release",
		Passwd:     "/etc/passwd",
		Shells:     "/etc/shells",
		MachineID:  "/etc/machine-id",
		SSHDConfig: "/etc/ssh/sshd_config",
	}
}

// Discover collects host facts.
//
// Every source is best-effort and independently optional. A host missing
// /etc/os-release still reports its accounts, because partial facts still close
// part of the gap — refusing to report anything unless everything is readable
// would make the least conventional hosts the least visible, which is backwards.
func Discover(cfg DiscoverConfig) Facts {
	f := Facts{SSHPorts: sshPorts(cfg.SSHDConfig)}

	if h, err := os.Hostname(); err == nil {
		f.Hostname = h
		if fqdn, err := net.LookupCNAME(h); err == nil {
			f.FQDN = strings.TrimSuffix(fqdn, ".")
		}
	}
	if b, err := os.ReadFile(cfg.MachineID); err == nil {
		f.MachineID = strings.TrimSpace(string(b))
	}
	f.OS = osName(cfg.OSRelease)
	f.Addresses = addresses()
	f.Accounts = loginAccounts(cfg.Passwd, cfg.Shells)
	return f
}

// osName reads PRETTY_NAME from an os-release file.
func osName(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()

	sc := bufio.NewScanner(file)
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if ok && k == "PRETTY_NAME" {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

// addresses lists non-loopback unicast addresses.
func addresses() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	out := []string{}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() {
			continue
		}
		out = append(out, ipnet.IP.String())
	}
	return out
}

// sshPorts reads the Port directives from an sshd config.
//
// Falls back to 22 when the file says nothing, because that is what sshd itself
// does — reporting no ports for a default configuration would read as "sshd is
// not listening", which is the opposite of the truth.
func sshPorts(path string) []int {
	file, err := os.Open(path)
	if err != nil {
		return []int{22}
	}
	defer file.Close()

	seen := map[int]bool{}
	out := []int{}
	add := func(p int) {
		if p > 0 && p < 65536 && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}

	sc := bufio.NewScanner(file)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		switch strings.ToLower(fields[0]) {
		case "port":
			if p, err := strconv.Atoi(fields[1]); err == nil {
				add(p)
			}
		case "listenaddress":
			// ListenAddress may carry its own port, as host:port or [v6]:port.
			// A port declared only here is still a way in.
			if _, portStr, err := net.SplitHostPort(fields[1]); err == nil {
				if p, err := strconv.Atoi(portStr); err == nil {
					add(p)
				}
			}
		}
	}
	if len(out) == 0 {
		return []int{22}
	}
	return out
}

// loginAccounts lists accounts with a shell that can hold a session.
//
// Only the name and shell are read. Accounts with nologin or false as their
// shell are excluded because they cannot be the principal of an interactive
// session, and listing them would bury the handful that matter in a hundred
// service accounts.
func loginAccounts(passwdPath, shellsPath string) []string {
	valid := validShells(shellsPath)

	file, err := os.Open(passwdPath)
	if err != nil {
		return nil
	}
	defer file.Close()

	out := []string{}
	sc := bufio.NewScanner(file)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ":")
		if len(fields) < 7 {
			continue
		}
		name, shell := fields[0], fields[6]
		base := shell[strings.LastIndex(shell, "/")+1:]
		if base == "nologin" || base == "false" || shell == "" {
			continue
		}
		// When /etc/shells is readable, trust it. When it is not, any shell
		// that is not an explicit refusal counts — a host with an unusual login
		// shell must not silently drop out of the account list.
		if len(valid) > 0 && !valid[shell] {
			continue
		}
		out = append(out, name)
	}
	return out
}

func validShells(path string) map[string]bool {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()

	out := map[string]bool{}
	sc := bufio.NewScanner(file)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			out[line] = true
		}
	}
	return out
}
