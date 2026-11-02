//go:build linux

package agent

import (
	"bufio"
	"context"
	"encoding/hex"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// appScanPaths are the files the Linux scan reads, swapped in tests.
type appScanPaths struct {
	proc       string // /proc
	cgroup     string // /sys/fs/cgroup
	dpkgStatus string
	apkDB      string
}

var defaultAppScanPaths = appScanPaths{
	proc:       "/proc",
	cgroup:     "/sys/fs/cgroup",
	dpkgStatus: "/var/lib/dpkg/status",
	apkDB:      "/lib/apk/db/installed",
}

func platformAppScan(ctx context.Context) hostScan {
	return scanLinux(ctx, defaultAppScanPaths)
}

func scanLinux(ctx context.Context, p appScanPaths) hostScan {
	var raw hostScan
	if ls, ok := listeningSockets(p.proc); ok {
		raw.listeners = ls
		raw.sources = append(raw.sources, "sockets")
	}
	raw.processes = catalogueProcesses(p.proc)
	if svcs, ok := runningServices(p.cgroup); ok {
		raw.services = svcs
		raw.sources = append(raw.sources, "systemd")
	}
	if pkgs, src, ok := installedPackages(ctx, p); ok {
		raw.packages = pkgs
		raw.sources = append(raw.sources, src)
	}
	return raw
}

// listeningSockets reads the kernel's TCP tables and names the process
// behind each listening socket.
func listeningSockets(proc string) ([]Listener, bool) {
	byInode := map[string]Listener{}
	read := false
	for _, f := range []string{"net/tcp", "net/tcp6"} {
		ls, err := parseProcNetTCP(filepath.Join(proc, f))
		if err != nil {
			continue
		}
		read = true
		for inode, l := range ls {
			byInode[inode] = l
		}
	}
	if !read {
		return nil, false
	}
	owners := socketOwners(proc, byInode)
	out := make([]Listener, 0, len(byInode))
	seen := map[ListenAddr]bool{}
	for inode, l := range byInode {
		if pid, ok := owners[inode]; ok {
			l.pid = pid
			l.Process = processName(proc, pid)
			l.Unit = processUnit(proc, pid)
		}
		// SO_REUSEPORT gives one address several sockets: one row is enough.
		k := ListenAddr{l.Address, l.Port}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, l)
	}
	return out, true
}

// parseProcNetTCP returns the listening sockets in one /proc/net/tcp* file,
// keyed by socket inode.
func parseProcNetTCP(path string) (map[string]Listener, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]Listener{}
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 || fields[3] != "0A" { // 0A is TCP_LISTEN
			continue
		}
		addr, port, ok := decodeProcAddr(fields[1])
		if !ok {
			continue
		}
		out[fields[9]] = Listener{Address: addr, Port: port}
	}
	return out, sc.Err()
}

// decodeProcAddr decodes "0100007F:1538" (IPv4) or its 32-digit IPv6 form.
// The kernel prints each 32-bit word in host byte order.
func decodeProcAddr(s string) (string, int, bool) {
	h, p, ok := strings.Cut(s, ":")
	if !ok {
		return "", 0, false
	}
	port, err := strconv.ParseUint(p, 16, 16)
	if err != nil {
		return "", 0, false
	}
	b, err := hex.DecodeString(h)
	if err != nil || (len(b) != 4 && len(b) != 16) {
		return "", 0, false
	}
	ip := make(net.IP, len(b))
	for i := 0; i < len(b); i += 4 {
		ip[i], ip[i+1], ip[i+2], ip[i+3] = b[i+3], b[i+2], b[i+1], b[i]
	}
	if v4 := ip.To4(); v4 != nil && len(b) == 16 && !ip.IsUnspecified() {
		ip = v4
	}
	return ip.String(), int(port), true
}

// socketOwners maps each socket inode to the pid holding it, by reading the
// fd symlinks under /proc. Stops as soon as every socket has an owner.
func socketOwners(proc string, sockets map[string]Listener) map[string]int {
	out := map[string]int{}
	ents, err := os.ReadDir(proc)
	if err != nil {
		return out
	}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		fdDir := filepath.Join(proc, e.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue
			}
			inode, ok := strings.CutPrefix(target, "socket:[")
			if !ok {
				continue
			}
			inode = strings.TrimSuffix(inode, "]")
			if _, want := sockets[inode]; want {
				if _, have := out[inode]; !have {
					out[inode] = pid
				}
			}
		}
		if len(out) == len(sockets) {
			break
		}
	}
	return out
}

// catalogueProcesses lists the running processes the catalogue names, once
// each, from every process's name.
func catalogueProcesses(proc string) []string {
	ents, err := os.ReadDir(proc)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	out := []string{}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		name := processName(proc, pid)
		if seen[name] || kindByProcess(name) == "" {
			continue
		}
		// A container's processes show in the host's /proc too. The
		// runtime reports those, with their image and published ports; a
		// host Postgres inferred from a container's would be a phantom.
		if b, err := os.ReadFile(filepath.Join(proc, e.Name(), "cgroup")); err == nil && inContainer(string(b)) {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// processName is a process's executable name. comm is cut at 15 characters,
// so the executable's basename wins when it starts with comm. The command
// line is never read: it is where passwords passed as flags live.
func processName(proc string, pid int) string {
	dir := filepath.Join(proc, strconv.Itoa(pid))
	comm := ""
	if b, err := os.ReadFile(filepath.Join(dir, "comm")); err == nil {
		comm = strings.TrimSpace(string(b))
	}
	if exe, err := os.Readlink(filepath.Join(dir, "exe")); err == nil {
		base := filepath.Base(strings.TrimSuffix(exe, " (deleted)"))
		if comm == "" || strings.HasPrefix(base, comm) {
			return base
		}
	}
	return comm
}

// processUnit is the systemd service a process runs under, from its cgroup.
func processUnit(proc string, pid int) string {
	b, err := os.ReadFile(filepath.Join(proc, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return ""
	}
	return unitOfCgroup(string(b))
}

// inContainer is whether a cgroup path is a container's.
func inContainer(cgroup string) bool {
	for _, marker := range []string{"/docker-", "/docker/", "libpod-", "/kubepods", "cri-containerd-", "/lxc.payload", "/machine.slice/"} {
		if strings.Contains(cgroup, marker) {
			return true
		}
	}
	return false
}

func unitOfCgroup(cgroup string) string {
	for _, line := range strings.Split(cgroup, "\n") {
		// "0::/system.slice/postgresql@16-main.service" (v2) or
		// "1:name=systemd:/system.slice/nginx.service" (v1).
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 || (parts[0] != "0" && parts[1] != "name=systemd") {
			continue
		}
		for _, seg := range strings.Split(parts[2], "/") {
			if strings.HasSuffix(seg, ".service") {
				return seg
			}
		}
	}
	return ""
}

// runningServices lists systemd services with processes in them, from the
// cgroup tree, without asking systemd over D-Bus.
func runningServices(cgroup string) ([]string, bool) {
	for _, dir := range []string{
		filepath.Join(cgroup, "system.slice"),
		filepath.Join(cgroup, "systemd", "system.slice"),
	} {
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		out := []string{}
		for _, e := range ents {
			if !e.IsDir() || !strings.HasSuffix(e.Name(), ".service") {
				continue
			}
			if populated(filepath.Join(dir, e.Name())) {
				out = append(out, e.Name())
			}
		}
		return out, true
	}
	return nil, false
}

// populated is whether a cgroup holds any process. A oneshot service that has
// exited can leave its cgroup behind empty.
func populated(dir string) bool {
	if b, err := os.ReadFile(filepath.Join(dir, "cgroup.events")); err == nil {
		return strings.Contains(string(b), "populated 1")
	}
	b, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	return err == nil && len(strings.TrimSpace(string(b))) > 0
}

// installedPackages reads the package database: dpkg's status file, Alpine's
// installed file, or rpm when neither is there.
func installedPackages(ctx context.Context, p appScanPaths) ([]installedPackage, string, bool) {
	if pkgs, err := parseStanzas(p.dpkgStatus, "Package", "Version", "Status"); err == nil {
		return pkgs, "dpkg", true
	}
	if pkgs, err := parseStanzas(p.apkDB, "P", "V", ""); err == nil {
		return pkgs, "apk", true
	}
	if pkgs, err := rpmPackages(ctx); err == nil {
		return pkgs, "rpm", true
	}
	return nil, "", false
}

// parseStanzas reads a file of blank-line separated "Key: value" stanzas.
// With statusKey set, only stanzas whose status ends in "installed" count.
func parseStanzas(path, nameKey, versionKey, statusKey string) ([]installedPackage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := []installedPackage{}
	var cur installedPackage
	status := ""
	flush := func() {
		if cur.name != "" && kindByPackage(cur.name) != "" &&
			(statusKey == "" || strings.HasSuffix(status, " installed")) {
			out = append(out, cur)
		}
		cur, status = installedPackage{}, ""
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			flush()
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.HasPrefix(line, " ") {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case nameKey:
			cur.name = v
		case versionKey:
			cur.version = v
		case statusKey:
			status = v
		}
	}
	flush()
	return out, sc.Err()
}

func rpmPackages(ctx context.Context) ([]installedPackage, error) {
	path, err := exec.LookPath("rpm")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, path, "-qa", "--qf", `%{NAME}\t%{VERSION}-%{RELEASE}\n`).Output()
	if err != nil {
		return nil, err
	}
	out := []installedPackage{}
	for _, line := range strings.Split(string(b), "\n") {
		name, version, ok := strings.Cut(line, "\t")
		if ok && kindByPackage(name) != "" {
			out = append(out, installedPackage{name: name, version: version})
		}
	}
	return out, nil
}
