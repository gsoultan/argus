package agent

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Where Postgres really is on this server, read from the server rather than
assumed. Adding a database used to sign in over /var/run/postgresql on port
5432 and give up; a Postgres on another port, a socket in /tmp, a second
cluster, one in a container, or no Postgres at all behind a PgBouncer, all
ended at "no such file or directory".

What is read, in order of trust:

  - every running postmaster (/proc/<pid>/cmdline names postgres or
    postmaster as the program, which its children rewrite away): its data
    directory from -D, PGDATA or its working directory, and the
    postmaster.pid there, whose lines 4 and 5 are the port and the first
    socket directory it actually opened. A postmaster in a container is read
    through /proc/<pid>/root.
  - its postgresql.conf, with include, include_if_exists and include_dir,
    then postgresql.auto.conf, then the -p, -k and -c options it was started
    with, when postmaster.pid cannot be read.
  - Debian's clusters, /etc/postgresql/<version>/<cluster>/postgresql.conf,
    which is what pg_lsclusters lists.
  - every bound socket named .s.PGSQL.<port> in /proc/net/unix and in the
    usual directories, unless PgBouncer is the one listening on it.
  - PgBouncer's pgbouncer.ini (the running one's and /etc/pgbouncer's): its
    own port, so that is never taken for Postgres's, and the [databases] it
    sends on, which say where Postgres is when it is on another server.

Nothing here signs in; dbdiscover.go tries what was found.
*/

// pgPlace is one way into a Postgres on this server.
type pgPlace struct {
	// Host is a socket directory, or 127.0.0.1 for TCP.
	Host string
	Port int
	// Label says which Postgres: "16/main", "pid 812", "in a container".
	Label string
	// From says where it was read, for the message when nothing answers.
	From string
	// Container marks a postmaster in another mount namespace; its socket
	// is reached through /proc/<pid>/root and peer authentication there
	// rarely knows this server's postgres account.
	Container bool
}

func (p pgPlace) String() string {
	if strings.HasPrefix(p.Host, "/") {
		return fmt.Sprintf("%s (port %d)", p.Host, p.Port)
	}
	return net2(p.Host, p.Port)
}

func net2(host string, port int) string {
	if strings.Contains(host, ":") {
		return fmt.Sprintf("[%s]:%d", host, port)
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// pgBouncerSeen is one PgBouncer's configuration.
type pgBouncerSeen struct {
	Config string
	Port   int
	// Socket is its unix_socket_dir; empty when it listens on TCP only.
	Socket string
	// Targets are its [databases]: where each name is sent.
	Targets []tunnel.PoolerTarget
}

// pgLocation is everything found.
type pgLocation struct {
	Places   []pgPlace
	Bouncers []pgBouncerSeen
}

// pgLocator reads the server under root, "/" in production.
type pgLocator struct {
	root string
}

func (l pgLocator) path(p string) string {
	if l.root == "" || l.root == "/" {
		return p
	}
	return filepath.Join(l.root, p)
}

func (l pgLocator) read(p string, max int64) ([]byte, error) {
	f, err := os.Open(l.path(p))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var b bytes.Buffer
	_, err = b.ReadFrom(io.LimitReader(f, max))
	return b.Bytes(), err
}

// sameDir is a socket directory with symlinks resolved, so /run/postgresql
// and /var/run/postgresql are one place.
func (l pgLocator) sameDir(dir string) string {
	if !strings.HasPrefix(dir, "/") {
		return dir
	}
	if r, err := filepath.EvalSymlinks(l.path(dir)); err == nil {
		return r
	}
	return dir
}

// socketExists says whether a Postgres socket is bound in dir for port.
func (l pgLocator) socketExists(dir string, port int) bool {
	st, err := os.Stat(l.path(filepath.Join(dir, fmt.Sprintf(".s.PGSQL.%d", port))))
	return err == nil && st.Mode()&os.ModeSocket != 0
}

func (l pgLocator) locate() pgLocation {
	var loc pgLocation
	seen := map[string]bool{}
	add := func(p pgPlace) {
		if p.Port < 1 || p.Port > 65535 || p.Host == "" {
			return
		}
		k := l.sameDir(p.Host) + "\x00" + strconv.Itoa(p.Port)
		if seen[k] {
			return
		}
		seen[k] = true
		loc.Places = append(loc.Places, p)
	}

	procs := l.processes()
	owners := l.socketOwners(procs)

	// PgBouncer first: its port must never be taken for Postgres's.
	bouncerPorts := map[int]bool{}
	confs := []string{}
	for _, p := range procs {
		if p.comm != "pgbouncer" {
			continue
		}
		for _, a := range p.args[1:] {
			if strings.HasSuffix(a, ".ini") {
				confs = append(confs, l.inRoot(p, a))
			}
		}
	}
	confs = append(confs, "/etc/pgbouncer/pgbouncer.ini", "/etc/pgbouncer.ini", "/usr/local/etc/pgbouncer.ini")
	doneConf := map[string]bool{}
	for _, c := range confs {
		if doneConf[c] {
			continue
		}
		doneConf[c] = true
		if b, ok := l.pgbouncer(c); ok {
			loc.Bouncers = append(loc.Bouncers, b)
			bouncerPorts[b.Port] = true
		}
	}

	// Running postmasters.
	for _, p := range procs {
		if !p.postmaster {
			continue
		}
		for _, pl := range l.fromPostmaster(p) {
			add(pl)
		}
	}

	// Debian and Ubuntu clusters.
	confsDeb, _ := filepath.Glob(l.path("/etc/postgresql/*/*/postgresql.conf"))
	sort.Strings(confsDeb)
	for _, c := range confsDeb {
		rel := c
		if l.root != "" && l.root != "/" {
			rel = "/" + strings.TrimPrefix(strings.TrimPrefix(c, l.root), "/")
		}
		parts := strings.Split(filepath.Dir(rel), string(filepath.Separator))
		if len(parts) < 2 {
			continue
		}
		label := parts[len(parts)-2] + "/" + parts[len(parts)-1]
		s := l.readConf(rel, nil)
		port := atoiOr(s["port"], 5432)
		dirs := socketDirs(s["unix_socket_directories"], "/var/run/postgresql")
		for _, d := range dirs {
			add(pgPlace{Host: d, Port: port, Label: label, From: rel})
		}
	}

	// Bound sockets nobody's configuration explained.
	for _, s := range l.unixSockets() {
		if owners[s.inode] == "pgbouncer" || (owners[s.inode] == "" && bouncerPorts[s.port]) {
			continue
		}
		add(pgPlace{Host: s.dir, Port: s.port, From: "a socket in " + s.dir})
	}
	for _, d := range []string{"/var/run/postgresql", "/run/postgresql", "/tmp", "/var/pgsql_socket"} {
		ms, _ := filepath.Glob(l.path(filepath.Join(d, ".s.PGSQL.*")))
		sort.Strings(ms)
		for _, m := range ms {
			port, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(m), ".s.PGSQL."))
			if err != nil || bouncerPorts[port] || !l.socketExists(d, port) {
				continue
			}
			add(pgPlace{Host: d, Port: port, From: "a socket in " + d})
		}
	}
	// A Postgres that is running sorts ahead of a cluster only configured.
	sort.SliceStable(loc.Places, func(i, j int) bool {
		return l.live(loc.Places[i]) && !l.live(loc.Places[j])
	})
	return loc
}

// live says whether a place has its socket bound, or is reached by TCP.
func (l pgLocator) live(p pgPlace) bool {
	return !strings.HasPrefix(p.Host, "/") || l.socketExists(p.Host, p.Port)
}

type proc struct {
	pid        string
	comm       string
	args       []string
	postmaster bool
}

func (l pgLocator) processes() []proc {
	ents, err := os.ReadDir(l.path("/proc"))
	if err != nil {
		return nil
	}
	var out []proc
	for _, e := range ents {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		b, err := l.read("/proc/"+e.Name()+"/cmdline", 1<<16)
		if err != nil || len(b) == 0 {
			continue
		}
		args := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
		prog := filepath.Base(args[0])
		p := proc{pid: e.Name(), comm: prog, args: args}
		if c, err := l.read("/proc/"+e.Name()+"/comm", 64); err == nil {
			p.comm = strings.TrimSpace(string(c))
		}
		// A postmaster's children rewrite argv[0] to "postgres: ...", so
		// only the postmaster itself is named by a bare program.
		p.postmaster = prog == "postgres" || prog == "postmaster"
		if prog == "pgbouncer" {
			p.comm = "pgbouncer"
		}
		out = append(out, p)
	}
	return out
}

// inContainer says whether p sees another root than this process.
func (l pgLocator) inContainer(p proc) bool {
	mine, err1 := os.Readlink(l.path("/proc/self/ns/mnt"))
	its, err2 := os.Readlink(l.path("/proc/" + p.pid + "/ns/mnt"))
	return err1 == nil && err2 == nil && mine != its
}

// inRoot is a path as p sees it, as this process can reach it.
func (l pgLocator) inRoot(p proc, path string) string {
	if l.inContainer(p) {
		return "/proc/" + p.pid + "/root" + path
	}
	return path
}

func (l pgLocator) fromPostmaster(p proc) []pgPlace {
	opts := map[string]string{}
	var dataDir string
	for i := 1; i < len(p.args); i++ {
		a := p.args[i]
		next := func() string {
			if i+1 < len(p.args) {
				i++
				return p.args[i]
			}
			return ""
		}
		switch {
		case a == "-D":
			dataDir = next()
		case strings.HasPrefix(a, "-D"):
			dataDir = a[2:]
		case a == "-p":
			opts["port"] = next()
		case strings.HasPrefix(a, "-p"):
			opts["port"] = a[2:]
		case a == "-k":
			opts["unix_socket_directories"] = next()
		case strings.HasPrefix(a, "-k"):
			opts["unix_socket_directories"] = a[2:]
		case a == "-c":
			k, v, _ := strings.Cut(next(), "=")
			opts[normGUC(k)] = v
		case strings.HasPrefix(a, "--"):
			k, v, _ := strings.Cut(a[2:], "=")
			opts[normGUC(k)] = v
		}
	}
	if dataDir == "" {
		if env, err := l.read("/proc/"+p.pid+"/environ", 1<<16); err == nil {
			for _, kv := range strings.Split(string(env), "\x00") {
				if v, ok := strings.CutPrefix(kv, "PGDATA="); ok {
					dataDir = v
				}
			}
		}
	}
	if dataDir == "" {
		dataDir, _ = os.Readlink(l.path("/proc/" + p.pid + "/cwd"))
	}
	ctr := l.inContainer(p)
	label := "pid " + p.pid
	if ctr {
		label = "in a container, pid " + p.pid
	}
	here := func(dir string) string {
		if ctr {
			return "/proc/" + p.pid + "/root" + dir
		}
		return dir
	}

	// postmaster.pid is what the server opened, not what it was told.
	if dataDir != "" {
		if b, err := l.read(here(filepath.Join(dataDir, "postmaster.pid")), 4096); err == nil {
			lines := strings.Split(string(b), "\n")
			if len(lines) >= 5 {
				port, _ := strconv.Atoi(strings.TrimSpace(lines[3]))
				sock := strings.TrimSpace(lines[4])
				var out []pgPlace
				from := filepath.Join(dataDir, "postmaster.pid")
				if sock != "" {
					out = append(out, pgPlace{Host: here(sock), Port: port, Label: label, From: from, Container: ctr})
				}
				listen := ""
				if len(lines) >= 6 {
					listen = strings.TrimSpace(lines[5])
				}
				if listen != "" && !ctr {
					out = append(out, pgPlace{Host: loopbackFor(listen), Port: port, Label: label, From: from})
				}
				if len(out) > 0 {
					return out
				}
			}
		}
	}

	// Its configuration, then what it was started with.
	s := map[string]string{}
	conf := opts["config_file"]
	if conf == "" && dataDir != "" {
		conf = filepath.Join(dataDir, "postgresql.conf")
	}
	if conf != "" {
		s = l.readConf(here(conf), nil)
		if dd := s["data_directory"]; dd != "" && dataDir == "" {
			dataDir = dd
		}
		if dataDir != "" {
			for k, v := range l.readConf(here(filepath.Join(dataDir, "postgresql.auto.conf")), nil) {
				s[k] = v
			}
		}
	}
	for k, v := range opts {
		s[k] = v
	}
	port := atoiOr(s["port"], 5432)
	var out []pgPlace
	for _, d := range socketDirs(s["unix_socket_directories"], "/var/run/postgresql") {
		out = append(out, pgPlace{Host: here(d), Port: port, Label: label, From: conf, Container: ctr})
	}
	if la := s["listen_addresses"]; la != "" && !ctr {
		out = append(out, pgPlace{Host: loopbackFor(la), Port: port, Label: label, From: conf})
	}
	return out
}

// loopbackFor is how this server reaches a Postgres listening on listen.
func loopbackFor(listen string) string {
	first := strings.TrimSpace(strings.Split(listen, ",")[0])
	if first == "::1" || first == "::" {
		return "::1"
	}
	return "127.0.0.1"
}

func normGUC(k string) string { return strings.ReplaceAll(strings.ToLower(k), "-", "_") }

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
		return n
	}
	return def
}

// socketDirs splits unix_socket_directories; "@" names an abstract socket
// no directory holds, and an empty setting turns sockets off.
func socketDirs(v, def string) []string {
	if v == "" {
		return []string{def}
	}
	var out []string
	for _, d := range strings.Split(v, ",") {
		d = strings.TrimSpace(d)
		if strings.HasPrefix(d, "/") && !strings.Contains(d, "..") {
			out = append(out, filepath.Clean(d))
		}
	}
	return out
}

var confLine = regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_.]*)\s*(?:=\s*|\s+)(.*)$`)

// readConf reads a postgresql.conf the way Postgres does, as far as the
// settings asked here go: last one wins, includes are followed.
func (l pgLocator) readConf(path string, depth []string) map[string]string {
	out := map[string]string{}
	if len(depth) > 10 {
		return out
	}
	for _, d := range depth {
		if d == path {
			return out
		}
	}
	b, err := l.read(path, 1<<20)
	if err != nil {
		return out
	}
	depth = append(depth, path)
	rel := func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(filepath.Dir(path), p)
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		m := confLine.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		key, val := strings.ToLower(m[1]), confValue(m[2])
		switch key {
		case "include", "include_if_exists":
			for k, v := range l.readConf(rel(val), depth) {
				out[k] = v
			}
		case "include_dir":
			ms, _ := filepath.Glob(l.path(filepath.Join(rel(val), "*.conf")))
			sort.Strings(ms)
			for _, m := range ms {
				p := m
				if l.root != "" && l.root != "/" {
					p = "/" + strings.TrimPrefix(strings.TrimPrefix(m, l.root), "/")
				}
				for k, v := range l.readConf(p, depth) {
					out[k] = v
				}
			}
		default:
			out[key] = val
		}
	}
	return out
}

// confValue is a setting's value: single-quoted (a doubled quote is one
// quote), or a bare word,
// either way before any comment.
func confValue(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "'") {
		var b strings.Builder
		for i := 1; i < len(s); i++ {
			c := s[i]
			if c == '\\' && i+1 < len(s) {
				i++
				b.WriteByte(s[i])
				continue
			}
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					b.WriteByte('\'')
					i++
					continue
				}
				break
			}
			b.WriteByte(c)
		}
		return b.String()
	}
	if i := strings.IndexByte(s, '#'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

type boundSocket struct {
	dir   string
	port  int
	inode string
}

var pgSockName = regexp.MustCompile(`^(/.*)/\.s\.PGSQL\.(\d+)$`)

// unixSockets lists every bound Postgres-named socket in /proc/net/unix.
func (l pgLocator) unixSockets() []boundSocket {
	b, err := l.read("/proc/net/unix", 8<<20)
	if err != nil {
		return nil
	}
	var out []boundSocket
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		// Num RefCount Protocol Flags Type St Inode Path
		if len(f) < 8 {
			continue
		}
		m := pgSockName.FindStringSubmatch(f[7])
		if m == nil {
			continue
		}
		port, _ := strconv.Atoi(m[2])
		out = append(out, boundSocket{dir: m[1], port: port, inode: f[6]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].port < out[j].port })
	return out
}

// socketOwners maps a socket's inode to the program holding it, for the
// processes this agent may look into.
func (l pgLocator) socketOwners(procs []proc) map[string]string {
	out := map[string]string{}
	for _, p := range procs {
		if !p.postmaster && p.comm != "pgbouncer" {
			continue
		}
		fds, err := os.ReadDir(l.path("/proc/" + p.pid + "/fd"))
		if err != nil {
			continue
		}
		name := "postgres"
		if p.comm == "pgbouncer" {
			name = "pgbouncer"
		}
		for _, fd := range fds {
			t, err := os.Readlink(l.path("/proc/" + p.pid + "/fd/" + fd.Name()))
			if err != nil {
				continue
			}
			if in, ok := strings.CutPrefix(t, "socket:["); ok {
				out[strings.TrimSuffix(in, "]")] = name
			}
		}
	}
	return out
}

// pgbouncer reads a pgbouncer.ini: its own port and socket, and [databases].
func (l pgLocator) pgbouncer(path string) (pgBouncerSeen, bool) {
	b := pgBouncerSeen{Config: path, Port: 6432, Socket: "/tmp"}
	ok := false
	l.readIni(path, 0, func(section, key, val string) {
		ok = true
		switch section {
		case "pgbouncer":
			switch key {
			case "listen_port":
				b.Port = atoiOr(val, 6432)
			case "unix_socket_dir":
				b.Socket = val
			}
		case "databases":
			b.Targets = append(b.Targets, poolerTarget(key, val))
		}
	})
	if !ok {
		return b, false
	}
	return b, true
}

func (l pgLocator) readIni(path string, depth int, each func(section, key, val string)) {
	if depth > 5 {
		return
	}
	data, err := l.read(path, 1<<20)
	if err != nil {
		return
	}
	section := ""
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == ';' || line[0] == '#' {
			continue
		}
		if inc, ok := strings.CutPrefix(line, "%include"); ok {
			p := strings.TrimSpace(inc)
			if !filepath.IsAbs(p) {
				p = filepath.Join(filepath.Dir(path), p)
			}
			l.readIni(p, depth+1, func(s, k, v string) {
				if s == "" {
					s = section
				}
				each(s, k, v)
			})
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			continue
		}
		k, v, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		k = strings.TrimSpace(k)
		if strings.HasPrefix(k, `"`) && strings.HasSuffix(k, `"`) && len(k) >= 2 {
			k = k[1 : len(k)-1]
		} else {
			k = strings.ToLower(k)
		}
		each(section, k, strings.TrimSpace(v))
	}
}

// poolerTarget reads one [databases] line: name = host=... port=... dbname=...
// Settings left out are PgBouncer's defaults: its own socket, port 5432 and
// the same name.
func poolerTarget(name, conn string) tunnel.PoolerTarget {
	t := tunnel.PoolerTarget{Name: name, Port: 5432}
	for k, v := range parseConnStr(conn) {
		switch k {
		case "host":
			t.Host = v
		case "port":
			t.Port = atoiOr(v, 5432)
		case "dbname":
			t.DBName = v
		}
	}
	return t
}

// parseConnStr reads libpq's key=value list, values single-quoted or bare.
func parseConnStr(s string) map[string]string {
	out := map[string]string{}
	i := 0
	for i < len(s) {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		j := i
		for j < len(s) && s[j] != '=' && s[j] != ' ' {
			j++
		}
		key := strings.ToLower(s[i:j])
		for j < len(s) && s[j] == ' ' {
			j++
		}
		if j >= len(s) || s[j] != '=' {
			break
		}
		j++
		for j < len(s) && s[j] == ' ' {
			j++
		}
		var v strings.Builder
		if j < len(s) && s[j] == '\'' {
			j++
			for j < len(s) && s[j] != '\'' {
				if s[j] == '\\' && j+1 < len(s) {
					j++
				}
				v.WriteByte(s[j])
				j++
			}
			j++
		} else {
			for j < len(s) && s[j] != ' ' && s[j] != '\t' {
				v.WriteByte(s[j])
				j++
			}
		}
		if key != "" {
			out[key] = v.String()
		}
		i = j
	}
	return out
}
