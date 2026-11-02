package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

/*
What runs on a host: databases, brokers, web servers, container runtimes.

Argus already knows who can reach a server. Knowing that the server also runs
Postgres on 0.0.0.0:5432 is what turns "who can log in" into "who can reach
the data", and it is how a database gets added without anyone typing a port
from memory.

Read-only and secret-free, on purpose. The scan reads the kernel's socket
tables, a process's name (/proc/<pid>/comm, the basename of its executable),
the systemd unit a process belongs to, the package database, and the
container runtime's list of containers. It never reads a process's command
line or environment, a configuration file, a container's labels or its
environment -- those are where passwords live -- and it never connects to the
applications it finds. A name, a version, a port and an address are visible
to any local user already.
*/

// AppInventory is what an agent found running on its host.
type AppInventory struct {
	CheckedAt time.Time `json:"checked_at"`
	// Applications are the known applications found, one per kind on the host
	// and one per container.
	Applications []Application `json:"applications"`
	// Listeners are every TCP socket listening on the host, known or not. An
	// unrecognised process on a public port is worth seeing too.
	Listeners []Listener `json:"listeners"`
	// Containers are the containers the local Docker or Podman reports.
	Containers []Container `json:"containers"`
	// Services are the systemd services running on the host.
	Services []string `json:"services"`
	// Sources says which of the above the agent could read, so the console can
	// tell "no containers" from "could not ask".
	Sources []string `json:"sources"`
}

// Application is one known application found on a host.
type Application struct {
	// Kind is the catalogue key: postgres, redis, rabbitmq, nginx, docker...
	Kind string `json:"kind"`
	// Name is how people call it: PostgreSQL, Redis, RabbitMQ.
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	// Running is whether a process or service for it is up. A package that is
	// installed and stopped is still reported, as not running.
	Running bool `json:"running"`
	// Listen is where it accepts TCP connections.
	Listen  []ListenAddr `json:"listen,omitempty"`
	Process string       `json:"process,omitempty"`
	Unit    string       `json:"unit,omitempty"`
	Package string       `json:"package,omitempty"`
	// Container and Image are set when it runs in a container.
	Container string `json:"container,omitempty"`
	Image     string `json:"image,omitempty"`
	// Evidence is how it was found: process, unit, package, container, port.
	Evidence []string `json:"evidence"`
}

// ListenAddr is one address an application accepts connections on.
type ListenAddr struct {
	Address string `json:"address"`
	Port    int    `json:"port"`
}

// Listener is one listening TCP socket.
type Listener struct {
	Address string `json:"address"`
	Port    int    `json:"port"`
	Process string `json:"process,omitempty"`
	Unit    string `json:"unit,omitempty"`
	Kind    string `json:"kind,omitempty"`

	pid int
}

// Container is one container the local runtime reports.
type Container struct {
	Name    string       `json:"name"`
	Image   string       `json:"image"`
	State   string       `json:"state"`
	Runtime string       `json:"runtime"`
	Ports   []ListenAddr `json:"ports,omitempty"`
	Kind    string       `json:"kind,omitempty"`
}

// appSpec is one entry of the catalogue of applications the agent names.
type appSpec struct {
	kind, name string
	// processes are process names (comm or executable basename).
	processes []string
	// units are systemd unit names without .service; a trailing * matches a
	// prefix, so postgresql* finds postgresql@16-main.
	units []string
	// packages matches installed package names.
	packages *regexp.Regexp
	// images are image names without registry, owner or tag.
	images []string
}

// appCatalogue is the applications the agent recognises. Ambiguous runtimes
// (java, beam.smp, python) are never matched by process alone: their unit,
// package or image says what they are.
var appCatalogue = []appSpec{
	{kind: "postgres", name: "PostgreSQL", processes: []string{"postgres", "postmaster"},
		units: []string{"postgresql*"}, images: []string{"postgres", "postgresql", "postgis", "timescaledb", "timescaledb-ha"},
		packages: regexp.MustCompile(`^postgresql(-?\d+(\.\d+)?)?(-server)?$`)},
	{kind: "pgbouncer", name: "PgBouncer", processes: []string{"pgbouncer"}, units: []string{"pgbouncer*"},
		images: []string{"pgbouncer"}, packages: regexp.MustCompile(`^pgbouncer$`)},
	{kind: "mysql", name: "MySQL", processes: []string{"mysqld"}, units: []string{"mysql", "mysqld"},
		images: []string{"mysql", "percona-server"}, packages: regexp.MustCompile(`^(mysql-server(-\d.*)?|mysql-community-server|percona-server-server)$`)},
	{kind: "mariadb", name: "MariaDB", processes: []string{"mariadbd"}, units: []string{"mariadb"},
		images: []string{"mariadb"}, packages: regexp.MustCompile(`^mariadb-server(-\d.*)?$`)},
	{kind: "mongodb", name: "MongoDB", processes: []string{"mongod"}, units: []string{"mongod", "mongodb"},
		images: []string{"mongo", "mongodb"}, packages: regexp.MustCompile(`^(mongodb-org-server|mongodb-server|mongodb)$`)},
	{kind: "redis", name: "Redis", processes: []string{"redis-server"}, units: []string{"redis", "redis-server", "redis@*"},
		images: []string{"redis", "redis-stack-server"}, packages: regexp.MustCompile(`^(redis-server|redis\d*)$`)},
	{kind: "valkey", name: "Valkey", processes: []string{"valkey-server"}, units: []string{"valkey", "valkey-server"},
		images: []string{"valkey"}, packages: regexp.MustCompile(`^(valkey|valkey-server)$`)},
	{kind: "memcached", name: "Memcached", processes: []string{"memcached"}, units: []string{"memcached"},
		images: []string{"memcached"}, packages: regexp.MustCompile(`^memcached$`)},
	{kind: "rabbitmq", name: "RabbitMQ", units: []string{"rabbitmq-server"},
		images: []string{"rabbitmq"}, packages: regexp.MustCompile(`^rabbitmq-server$`)},
	{kind: "kafka", name: "Kafka", units: []string{"kafka"}, images: []string{"kafka", "cp-kafka"}},
	{kind: "elasticsearch", name: "Elasticsearch", units: []string{"elasticsearch"},
		images: []string{"elasticsearch"}, packages: regexp.MustCompile(`^elasticsearch$`)},
	{kind: "opensearch", name: "OpenSearch", units: []string{"opensearch"},
		images: []string{"opensearch"}, packages: regexp.MustCompile(`^opensearch$`)},
	{kind: "clickhouse", name: "ClickHouse", processes: []string{"clickhouse-server", "clickhouse"}, units: []string{"clickhouse-server"},
		images: []string{"clickhouse-server", "clickhouse"}, packages: regexp.MustCompile(`^clickhouse-server$`)},
	{kind: "mssql", name: "SQL Server", processes: []string{"sqlservr"}, units: []string{"mssql-server"},
		images: []string{"mssql-server"}, packages: regexp.MustCompile(`^mssql-server$`)},
	{kind: "etcd", name: "etcd", processes: []string{"etcd"}, units: []string{"etcd"}, images: []string{"etcd"},
		packages: regexp.MustCompile(`^etcd(-server)?$`)},
	{kind: "nginx", name: "nginx", processes: []string{"nginx"}, units: []string{"nginx"},
		images: []string{"nginx", "nginx-unprivileged", "openresty"}, packages: regexp.MustCompile(`^nginx(-core|-full|-light|-extras)?$`)},
	{kind: "apache", name: "Apache httpd", processes: []string{"apache2", "httpd"}, units: []string{"apache2", "httpd"},
		images: []string{"httpd"}, packages: regexp.MustCompile(`^(apache2|httpd)$`)},
	{kind: "haproxy", name: "HAProxy", processes: []string{"haproxy"}, units: []string{"haproxy"},
		images: []string{"haproxy"}, packages: regexp.MustCompile(`^haproxy$`)},
	{kind: "caddy", name: "Caddy", processes: []string{"caddy"}, units: []string{"caddy"}, images: []string{"caddy"},
		packages: regexp.MustCompile(`^caddy$`)},
	{kind: "traefik", name: "Traefik", processes: []string{"traefik"}, units: []string{"traefik"}, images: []string{"traefik"}},
	{kind: "docker", name: "Docker", processes: []string{"dockerd"}, units: []string{"docker"},
		packages: regexp.MustCompile(`^(docker-ce|docker\.io|docker-engine|moby-engine|docker)$`)},
	{kind: "containerd", name: "containerd", processes: []string{"containerd"}, units: []string{"containerd"},
		packages: regexp.MustCompile(`^(containerd|containerd\.io)$`)},
	{kind: "podman", name: "Podman", units: []string{"podman"}, packages: regexp.MustCompile(`^podman$`)},
	{kind: "kubernetes", name: "Kubernetes node", processes: []string{"kubelet", "k3s", "k3s-server", "k3s-agent"},
		units: []string{"kubelet", "k3s", "k3s-agent", "rke2-server", "rke2-agent"}},
	{kind: "prometheus", name: "Prometheus", processes: []string{"prometheus"}, units: []string{"prometheus"},
		images: []string{"prometheus"}, packages: regexp.MustCompile(`^prometheus$`)},
	{kind: "grafana", name: "Grafana", processes: []string{"grafana", "grafana-server"}, units: []string{"grafana-server", "grafana"},
		images: []string{"grafana", "grafana-oss", "grafana-enterprise"}, packages: regexp.MustCompile(`^grafana(-enterprise)?$`)},
	{kind: "vault", name: "Vault", processes: []string{"vault"}, units: []string{"vault"}, images: []string{"vault"},
		packages: regexp.MustCompile(`^vault$`)},
	{kind: "minio", name: "MinIO", processes: []string{"minio"}, units: []string{"minio"}, images: []string{"minio"}},
}

func specOf(kind string) *appSpec {
	for i := range appCatalogue {
		if appCatalogue[i].kind == kind {
			return &appCatalogue[i]
		}
	}
	return nil
}

func kindByProcess(name string) string {
	for _, s := range appCatalogue {
		for _, p := range s.processes {
			if p == name {
				return s.kind
			}
		}
	}
	return ""
}

func kindByUnit(unit string) string {
	u := strings.TrimSuffix(unit, ".service")
	for _, s := range appCatalogue {
		for _, pat := range s.units {
			if p, ok := strings.CutSuffix(pat, "*"); ok {
				if strings.HasPrefix(u, p) {
					return s.kind
				}
			} else if u == pat {
				return s.kind
			}
		}
	}
	return ""
}

func kindByPackage(name string) string {
	for _, s := range appCatalogue {
		if s.packages != nil && s.packages.MatchString(name) {
			return s.kind
		}
	}
	return ""
}

// imageName is an image reference without registry, owner, tag or digest:
// docker.io/bitnami/postgresql:16 is postgresql.
func imageName(ref string) (name, tag string) {
	ref, _, _ = strings.Cut(ref, "@")
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		ref = ref[i+1:]
	}
	name, tag, _ = strings.Cut(ref, ":")
	return strings.ToLower(name), tag
}

func kindByImage(ref string) string {
	name, _ := imageName(ref)
	for _, s := range appCatalogue {
		for _, img := range s.images {
			if img == name {
				return s.kind
			}
		}
	}
	return ""
}

// installedPackage is one package the package manager says is installed.
type installedPackage struct{ name, version string }

// hostScan is the raw material the platform scan collects.
type hostScan struct {
	listeners []Listener
	// processes are the catalogue processes running, listening or not:
	// dockerd and containerd answer on unix sockets only.
	processes  []string
	services   []string
	packages   []installedPackage
	containers []Container
	sources    []string
}

// classify turns what the scan collected into applications.
func classify(raw hostScan, now time.Time) AppInventory {
	inv := AppInventory{
		CheckedAt:    now.UTC(),
		Applications: []Application{},
		Listeners:    []Listener{},
		Containers:   []Container{},
		Services:     []string{},
		Sources:      raw.sources,
	}
	byKind := map[string]*Application{}
	host := func(kind string) *Application {
		if a, ok := byKind[kind]; ok {
			return a
		}
		s := specOf(kind)
		a := &Application{Kind: kind, Name: s.name, Evidence: []string{}}
		byKind[kind] = a
		return a
	}
	evidence := func(a *Application, e string) {
		for _, x := range a.Evidence {
			if x == e {
				return
			}
		}
		a.Evidence = append(a.Evidence, e)
	}

	pkgs := append([]installedPackage(nil), raw.packages...)
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].name > pkgs[j].name })
	for _, p := range pkgs {
		if k := kindByPackage(p.name); k != "" {
			a := host(k)
			// The server package carries the version; prefer the first one
			// found, which the sort below makes the highest-numbered.
			if a.Package == "" {
				a.Package, a.Version = p.name, p.version
			}
			evidence(a, "package")
		}
	}
	for _, unit := range raw.services {
		inv.Services = append(inv.Services, unit)
		if k := kindByUnit(unit); k != "" {
			a := host(k)
			a.Running = true
			if a.Unit == "" {
				a.Unit = unit
			}
			evidence(a, "unit")
		}
	}
	for _, name := range raw.processes {
		if k := kindByProcess(name); k != "" {
			a := host(k)
			a.Running = true
			if a.Process == "" {
				a.Process = name
			}
			evidence(a, "process")
		}
	}
	// A port a container publishes is the container's, whatever process
	// (docker-proxy, rootlessport) holds it on the host.
	published := map[int]string{}
	for _, c := range raw.containers {
		for _, p := range c.Ports {
			published[p.Port] = kindByImage(c.Image)
		}
	}
	for _, l := range raw.listeners {
		if k, ok := published[l.Port]; ok {
			l.Kind = k
			inv.Listeners = append(inv.Listeners, l)
			continue
		}
		k := kindByProcess(l.Process)
		if k == "" && l.Unit != "" {
			k = kindByUnit(l.Unit)
		}
		if k != "" {
			l.Kind = k
			a := host(k)
			a.Running = true
			if a.Process == "" {
				a.Process = l.Process
			}
			if a.Unit == "" && l.Unit != "" {
				a.Unit = l.Unit
			}
			a.Listen = appendListen(a.Listen, ListenAddr{Address: l.Address, Port: l.Port})
			evidence(a, "process")
		}
		inv.Listeners = append(inv.Listeners, l)
	}

	for _, c := range raw.containers {
		if k := kindByImage(c.Image); k != "" {
			c.Kind = k
			_, tag := imageName(c.Image)
			if tag == "latest" {
				tag = ""
			}
			inv.Applications = append(inv.Applications, Application{
				Kind: k, Name: specOf(k).name, Version: tag,
				Running: c.State == "running", Listen: c.Ports,
				Container: c.Name, Image: c.Image, Evidence: []string{"container"},
			})
		}
		inv.Containers = append(inv.Containers, c)
	}
	// A runtime with containers is running even when its daemon's process
	// was not seen (a rootless Podman has none).
	if len(raw.containers) > 0 {
		rt := raw.containers[0].Runtime
		if specOf(rt) != nil {
			a := host(rt)
			a.Running = true
			evidence(a, "containers")
		}
	}

	for _, a := range byKind {
		inv.Applications = append(inv.Applications, *a)
	}
	sort.SliceStable(inv.Applications, func(i, j int) bool {
		a, b := inv.Applications[i], inv.Applications[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Container < b.Container
	})
	sort.Slice(inv.Listeners, func(i, j int) bool {
		a, b := inv.Listeners[i], inv.Listeners[j]
		if a.Port != b.Port {
			return a.Port < b.Port
		}
		return a.Address < b.Address
	})
	sort.Strings(inv.Services)
	return inv
}

func appendListen(list []ListenAddr, l ListenAddr) []ListenAddr {
	for _, x := range list {
		if x == l {
			return list
		}
	}
	return append(list, l)
}

// DiscoverApps scans this host for applications. Best-effort: each source is
// optional, and Sources says which ones answered.
func DiscoverApps(ctx context.Context) AppInventory {
	raw := platformAppScan(ctx)
	if cs, rt, ok := localContainers(ctx); ok {
		raw.containers = cs
		raw.sources = append(raw.sources, rt)
	}
	return classify(raw, time.Now())
}

// containerSockets are where Docker and Podman answer on this host.
var containerSockets = []struct{ runtime, path string }{
	{"docker", "/var/run/docker.sock"},
	{"podman", "/run/podman/podman.sock"},
}

// localContainers lists containers from the first runtime that answers. Only
// GET /containers/json is ever called: it carries names, images, state and
// published ports, and never a container's environment. Labels are dropped.
func localContainers(ctx context.Context) ([]Container, string, bool) {
	for _, s := range containerSockets {
		cs, err := listContainers(ctx, s.path)
		if err != nil {
			continue
		}
		for i := range cs {
			cs[i].Runtime = s.runtime
		}
		return cs, s.runtime, true
	}
	return nil, "", false
}

func listContainers(ctx context.Context, socket string) ([]Container, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/containers/json?all=1", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list containers: %s", resp.Status)
	}
	return decodeContainers(io.LimitReader(resp.Body, 8<<20))
}

// decodeContainers reads the runtime's container list, keeping only the
// fields Container carries.
func decodeContainers(r io.Reader) ([]Container, error) {
	var list []struct {
		Names []string
		Image string
		State string
		Ports []struct {
			IP          string
			PrivatePort int
			PublicPort  int
			Type        string
		}
	}
	if err := json.NewDecoder(r).Decode(&list); err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(list))
	for _, c := range list {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		ct := Container{Name: name, Image: c.Image, State: c.State}
		for _, p := range c.Ports {
			if p.PublicPort == 0 || (p.Type != "" && p.Type != "tcp") {
				continue
			}
			addr := p.IP
			if addr == "" {
				addr = "0.0.0.0"
			}
			ct.Ports = appendListen(ct.Ports, ListenAddr{Address: addr, Port: p.PublicPort})
		}
		out = append(out, ct)
	}
	return out, nil
}
