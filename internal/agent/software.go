package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Installing and removing software from the console: the questions.

The console asks, on the "software" tunnel service (tunnel/software.go),
what is installed, what the repositories offer, and what a change would do.
The agent answers by running `argus-agent software-query` as root, outside
its sandbox, in a unit of its own (software_linux.go), which reads the
question on stdin and prints the answer. A change itself is a package run
on the heartbeat (software_apply.go), so it survives restarts like an
update does.

Two lists decide what may never be taken away from the console, and the
agent applies both itself, so a console that asked anyway is still refused:

  - protected: the agent, the package manager, the init system, the C
    library, sudo, the SSH server people sign in with, the kernel that is
    running, and anything the distribution calls essential or required.
  - keeps data: database and broker servers, whose purge scripts can delete
    their data directories. They can be removed; their purge is not offered.
*/

const softwareQueryTimeout = 3 * time.Minute

// protectedPackages are never removed from the console, by name or glob.
var protectedPackages = map[string]string{
	"argus-agent":     "it is the Argus agent itself",
	"argus-control":   "it is the Argus control plane, and the helper this server is looked after through",
	"apt":             "it is the package manager",
	"dpkg":            "it is the package manager",
	"dnf":             "it is the package manager",
	"dnf-data":        "it is the package manager",
	"python3-dnf":     "it is the package manager",
	"yum":             "it is the package manager",
	"rpm":             "it is the package manager",
	"zypper":          "it is the package manager",
	"libzypp":         "it is the package manager",
	"systemd":         "it is the init system",
	"systemd-sysv":    "it is the init system",
	"init":            "it is the init system",
	"libc6":           "it is the C library everything runs on",
	"glibc":           "it is the C library everything runs on",
	"sudo":            "people use it to administer the server",
	"bash":            "it is the login shell",
	"coreutils":       "the system cannot run without it",
	"util-linux":      "the system cannot run without it",
	"login":           "people sign in with it",
	"passwd":          "accounts are managed with it",
	"base-files":      "the system cannot run without it",
	"openssh-server":  "people sign in to the server with it; removing it can lock them out",
	"openssh":         "people sign in to the server with it; removing it can lock them out",
	"ca-certificates": "every TLS connection, the agent's included, checks against it",
}

// protectedGlobs are protected by pattern.
var protectedGlobs = map[string]string{
	"kernel":         "it is a kernel; remove old kernels with the distribution's own tools",
	"kernel-core*":   "it is a kernel; remove old kernels with the distribution's own tools",
	"kernel-default": "it is a kernel; remove old kernels with the distribution's own tools",
	"systemd-*":      "it is part of the init system",
}

// keepsDataGlobs are packages whose purge can delete data.
var keepsDataGlobs = []string{"postgresql*", "mysql-server*", "mariadb-server*", "mongodb*", "redis-server*",
	"redis", "valkey*", "rabbitmq-server*", "elasticsearch*", "opensearch*", "kafka*", "etcd*", "influxdb*",
	"clickhouse-server*", "docker-ce*", "docker.io", "containerd*", "podman*", "slapd", "389-ds-base*",
	"bind9*", "gitlab-*", "minio*", "cassandra*", "couchdb*", "neo4j*"}

// protection is what the agent knows about this host that the lists above
// do not: what the distribution calls essential, and the running kernel.
type protection struct {
	essential map[string]bool
	kernel    string // the running kernel's package on apt
}

func (p protection) why(name string) string {
	base, _, _ := strings.Cut(name, ":")
	if why, ok := protectedPackages[base]; ok {
		return why
	}
	for g, why := range protectedGlobs {
		if ok, _ := path.Match(g, base); ok {
			return why
		}
	}
	if p.kernel != "" && (base == p.kernel || base == strings.Replace(p.kernel, "linux-image-", "linux-modules-", 1)) {
		return "it is the kernel the server is running"
	}
	if p.essential[base] {
		return "the distribution marks it essential"
	}
	return ""
}

func keepsData(name string) bool {
	base, _, _ := strings.Cut(name, ":")
	for _, g := range keepsDataGlobs {
		if ok, _ := path.Match(g, base); ok {
			return true
		}
	}
	return false
}

// hostProtection reads what this host's package manager calls essential,
// and which kernel runs.
func hostProtection(ctx context.Context, run cmdRunner, m pkgManager) protection {
	p := protection{essential: map[string]bool{}}
	switch m.(type) {
	case aptManager:
		if b, _, err := output(ctx, run, nil, "dpkg-query", "-W", "-f=${Package}\t${Essential}\t${Priority}\n"); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				f := strings.Split(line, "\t")
				if len(f) == 3 && (f[1] == "yes" || f[2] == "required") {
					p.essential[f[0]] = true
				}
			}
		}
		if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
			p.kernel = "linux-image-" + strings.TrimSpace(string(b))
		}
	case dnfManager:
		// dnf's own list of what it refuses to remove.
		for _, dir := range []string{"/etc/dnf/protected.d", "/usr/share/dnf5/libdnf.conf.d", "/etc/yum/protected.d"} {
			ents, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, e := range ents {
				if !strings.HasSuffix(e.Name(), ".conf") {
					continue
				}
				if b, err := os.ReadFile(dir + "/" + e.Name()); err == nil {
					for _, n := range strings.Fields(string(b)) {
						if tunnel.ValidPackageName(n) {
							p.essential[n] = true
						}
					}
				}
			}
		}
	}
	return p
}

/* ── The helper ──────────────────────────────────────────────────────────── */

// RunSoftwareQuery is `argus-agent software-query`: one question on stdin,
// one answer on stdout.
func RunSoftwareQuery(stdin io.Reader, stdout io.Writer) int {
	var q tunnel.GrantSoftware
	if err := json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(&q); err != nil {
		_ = json.NewEncoder(stdout).Encode(tunnel.SoftwareResult{Error: "the question cannot be read"})
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), softwareQueryTimeout)
	defer cancel()
	res := SoftwareAsker{}.Ask(ctx, q)
	_ = json.NewEncoder(stdout).Encode(res)
	return 0
}

// SoftwareAsker answers questions about software with the host's package
// manager. Run and Look are replaced in tests.
type SoftwareAsker struct {
	Run  cmdRunner
	Look lookPathFunc
}

// Ask answers one question.
func (s SoftwareAsker) Ask(ctx context.Context, q tunnel.GrantSoftware) tunnel.SoftwareResult {
	if s.Run == nil {
		s.Run = execRunner
	}
	if s.Look == nil {
		s.Look = exec.LookPath
	}
	if err := q.Validate(); err != nil {
		return tunnel.SoftwareResult{Error: err.Error()}
	}
	pm := detectManager(s.Look)
	m := asSW(pm)
	if m == nil {
		return tunnel.SoftwareResult{Error: errNoManager.Error()}
	}
	res := tunnel.SoftwareResult{Manager: m.name()}
	switch q.Op {
	case tunnel.SoftwareInstalled:
		pkgs, err := m.installedList(ctx, s.Run)
		if err != nil {
			res.Error = err.Error()
			return res
		}
		prot := hostProtection(ctx, s.Run, m)
		res.Total = len(pkgs)
		out := make([]tunnel.InstalledPackage, 0, len(pkgs))
		for _, p := range pkgs {
			if p.Manual {
				res.Manual++
			}
			if !q.All && !p.Manual {
				continue
			}
			p.Protected = prot.why(p.Name)
			p.KeepsData = keepsData(p.Name)
			out = append(out, p)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		if len(out) > tunnel.MaxSoftwareInstalled {
			out, res.Truncated = out[:tunnel.MaxSoftwareInstalled], true
		}
		res.Installed, res.OK = out, true
	case tunnel.SoftwareSearch:
		found, err := m.search(ctx, s.Run, strings.TrimSpace(q.Query))
		if err != nil {
			res.Error = err.Error()
			return res
		}
		if found == nil {
			found = []tunnel.FoundPackage{}
		}
		res.Found, res.OK = found, true
	case tunnel.SoftwarePreview:
		if q.Action == tunnel.SoftwareInstall {
			refreshIfOld(ctx, s.Run, m)
		}
		pv, _, err := previewSoftware(ctx, s.Run, m, q.Action, q.Items, q.Purge)
		if err != nil {
			res.Error = err.Error()
			return res
		}
		res.Preview, res.OK = &pv, true
	}
	return res
}

// aptListsDir is where apt keeps the lists; their age says whether to
// refresh before an install is worked out. Tests move it.
var aptListsDir = "/var/lib/apt/lists"

// refreshIfOld refreshes apt's lists when they are over six hours old, so a
// preview is of what would really be installed. dnf and zypper refresh
// expired metadata on their own.
func refreshIfOld(ctx context.Context, run cmdRunner, m pkgManager) {
	if _, ok := m.(aptManager); !ok {
		return
	}
	st, err := os.Stat(aptListsDir)
	if err == nil && time.Since(st.ModTime()) < packageCheckEvery {
		return
	}
	_ = m.refresh(ctx, run, nil)
}

// previewSoftware simulates a change and says whether it may be done. It
// also returns the exact targets the real command is to be given.
func previewSoftware(ctx context.Context, run cmdRunner, m swManager, action string, items []tunnel.SoftwareItem,
	purge bool) (tunnel.SoftwarePreviewResult, []string, error) {
	pv := tunnel.SoftwarePreviewResult{Action: action, Items: items, Purge: purge, Changes: []tunnel.SoftwareChange{}}
	if err := tunnel.ValidateSoftwareChange(action, items, purge); err != nil {
		return pv, nil, err
	}
	if purge && !m.purges() {
		return pv, nil, fmt.Errorf("%s keeps configuration changed by hand as .rpmsave files; there is nothing to purge", m.name())
	}
	prot := hostProtection(ctx, run, m)
	asked := map[string]bool{}
	for _, it := range items {
		asked[it.Name] = true
		if action == tunnel.SoftwareRemove {
			if why := prot.why(it.Name); why != "" {
				pv.Blocked = fmt.Sprintf("Argus does not remove %s: %s.", it.Name, why)
				pv.Hash = changesHash(pv)
				return pv, nil, nil
			}
		}
	}
	sim, err := m.simulate(ctx, run, action, items, purge)
	if err != nil {
		return pv, nil, err
	}
	pv.Changes, pv.DownloadKB, pv.DiskKB = sim.changes, sim.downloadKB, sim.diskKB
	pv.PurgeAllowed = m.purges() && action == tunnel.SoftwareRemove
	var removed []string
	for i := range pv.Changes {
		c := &pv.Changes[i]
		base, _, _ := strings.Cut(c.Name, ":")
		c.Asked = asked[c.Name] || asked[base]
		switch c.Kind {
		case tunnel.ChangeRemove:
			removed = append(removed, c.Name)
			if action == tunnel.SoftwareInstall && pv.Blocked == "" {
				pv.Blocked = fmt.Sprintf("Installing this would remove %s. Argus does not install what takes "+
					"something else away; sort out the conflict on the server first.", c.Name)
			}
			if why := prot.why(c.Name); why != "" && pv.Blocked == "" {
				pv.Blocked = fmt.Sprintf("Removing this would also remove %s, which Argus does not remove: %s.",
					c.Name, why)
			}
			if keepsData(c.Name) {
				pv.PurgeAllowed = false
			}
		case tunnel.ChangeDowngrade:
			pv.Warnings = append(pv.Warnings, fmt.Sprintf("%s goes back from %s to %s.", c.Name, c.From, c.To))
		}
	}
	if purge && !pv.PurgeAllowed && pv.Blocked == "" {
		pv.Blocked = "Purging is not offered here: a package removed keeps data, and its purge can delete it."
	}
	if action == tunnel.SoftwareInstall {
		for _, it := range items {
			found := false
			for _, c := range pv.Changes {
				if c.Name == it.Name || strings.HasPrefix(c.Name, it.Name+":") {
					found = true
				}
			}
			if !found {
				pv.Warnings = append(pv.Warnings, it.Name+" is already installed at that version.")
			}
		}
	}
	if action == tunnel.SoftwareRemove && len(removed) > 0 {
		for _, u := range m.unitsOf(ctx, run, removed) {
			pv.Services = append(pv.Services, u)
			if code, _ := run(ctx, nil, nil, "systemctl", "is-active", "--quiet", u); code == 0 {
				pv.Warnings = append(pv.Warnings, u+" is running; it stops.")
			}
		}
	}
	if len(pv.Changes) == 0 && pv.Blocked == "" {
		pv.Blocked = "Nothing would change."
	}
	pv.Hash = changesHash(pv)
	return pv, sim.targets, nil
}

// changesHash names what a preview says would change, and whether it is
// blocked, so a run can tell the server still answers the same.
func changesHash(pv tunnel.SoftwarePreviewResult) string {
	lines := make([]string, 0, len(pv.Changes)+2)
	for _, c := range pv.Changes {
		lines = append(lines, c.Kind+" "+c.Name+" "+c.From+" "+c.To)
	}
	sort.Strings(lines)
	lines = append([]string{pv.Action, fmt.Sprint(pv.Purge), pv.Blocked}, lines...)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

/* ── The service ─────────────────────────────────────────────────────────── */

// SoftwareService is the tunnel service name.
const SoftwareService = tunnel.SoftwareService

// SoftwareConfig is the software service.
type SoftwareConfig struct {
	Verifier *tunnel.GrantVerifier
	Log      *slog.Logger
	// Ask answers a question; nil runs the helper as root in a unit of its
	// own (or in process where the agent cannot, which says why).
	Ask func(ctx context.Context, q tunnel.GrantSoftware) tunnel.SoftwareResult
	// Executable is the agent binary the helper runs from.
	Executable string
}

// Software is the service that answers what is installed, what can be, and
// what a change would do.
func Software(cfg SoftwareConfig) TunnelService {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	if cfg.Ask == nil {
		cfg.Ask = hostSoftwareAsk(cfg.Executable)
	}
	return func(ctx context.Context, call *Call) {
		if call.GrantKeyErr != nil {
			call.Refuse(call.GrantKeyErr.Error())
			return
		}
		g, err := cfg.Verifier.Verify(call.GrantKey, call.Header.Grant)
		if err != nil {
			log.Warn("tunnel: refused a software question", "error", err)
			call.Refuse(err.Error())
			return
		}
		if !g.ForService(SoftwareService) || g.Software == nil {
			log.Warn("tunnel: refused a software question on a grant for another service",
				"service", g.Service, "for", g.Email)
			call.Refuse(tunnel.ErrGrant{Reason: "it is not for software"}.Error())
			return
		}
		q := *g.Software
		if err := q.Validate(); err != nil {
			call.Refuse(err.Error())
			return
		}
		if err := call.Accept(""); err != nil {
			return
		}
		cctx, cancel := context.WithTimeout(ctx, softwareQueryTimeout)
		res := cfg.Ask(cctx, q)
		cancel()
		log.Info("tunnel: answered a software question", "op", q.Op, "action", q.Action,
			"ok", res.OK, "for", g.Email)
		b, _ := json.Marshal(res)
		_ = tunnel.WriteFrame(call.Stream, tunnel.FrameSoftwareResult, b)
		_ = call.Stream.Close()
	}
}
