package agent

import (
	"strings"
	"testing"
	"time"
)

// A host running Postgres from a package, Redis in a container and an
// unrecognised process on a public port reports all three, and names each
// only from what it could see.
func TestClassifyNamesWhatRuns(t *testing.T) {
	raw := hostScan{
		listeners: []Listener{
			{Address: "0.0.0.0", Port: 5432, Process: "postgres", Unit: "postgresql@16-main.service"},
			{Address: "::", Port: 5432, Process: "postgres", Unit: "postgresql@16-main.service"},
			{Address: "0.0.0.0", Port: 6380, Process: "docker-proxy", Unit: "docker.service"},
			{Address: "0.0.0.0", Port: 9000, Process: "mystery"},
			// beam.smp is Erlang: not RabbitMQ on its own.
			{Address: "0.0.0.0", Port: 4369, Process: "beam.smp"},
		},
		services: []string{"postgresql@16-main.service", "docker.service", "cron.service"},
		packages: []installedPackage{
			{"postgresql", "16+257"}, {"postgresql-16", "16.4-1"}, {"docker-ce", "27.1"},
		},
		containers: []Container{{Name: "cache", Image: "docker.io/library/redis:7.2", State: "running",
			Runtime: "docker", Ports: []ListenAddr{{"0.0.0.0", 6380}}}},
		sources: []string{"sockets", "systemd", "dpkg", "docker"},
	}
	inv := classify(raw, time.Unix(0, 0))

	byKind := map[string]Application{}
	for _, a := range inv.Applications {
		byKind[a.Kind+"/"+a.Container] = a
	}
	pg, ok := byKind["postgres/"]
	if !ok || !pg.Running || pg.Version != "16.4-1" || pg.Package != "postgresql-16" ||
		pg.Unit != "postgresql@16-main.service" || len(pg.Listen) != 2 {
		t.Errorf("postgres = %+v", pg)
	}
	if r, ok := byKind["redis/cache"]; !ok || r.Version != "7.2" || !r.Running || r.Listen[0].Port != 6380 {
		t.Errorf("redis container = %+v", r)
	}
	if d, ok := byKind["docker/"]; !ok || !d.Running {
		t.Errorf("docker = %+v", d)
	}
	if _, ok := byKind["rabbitmq/"]; ok {
		t.Error("beam.smp alone was named RabbitMQ")
	}
	kinds := map[int]string{}
	for _, l := range inv.Listeners {
		kinds[l.Port] = l.Kind
	}
	if kinds[6380] != "redis" || kinds[9000] != "" || kinds[5432] != "postgres" {
		t.Errorf("listener kinds = %v", kinds)
	}
	if len(inv.Listeners) != 5 || len(inv.Services) != 3 {
		t.Errorf("listeners %d services %d", len(inv.Listeners), len(inv.Services))
	}
}

// An installed, stopped server is reported, as not running.
func TestClassifyInstalledButStopped(t *testing.T) {
	inv := classify(hostScan{packages: []installedPackage{{"rabbitmq-server", "3.13"}}}, time.Now())
	if len(inv.Applications) != 1 || inv.Applications[0].Running || inv.Applications[0].Kind != "rabbitmq" {
		t.Fatalf("got %+v", inv.Applications)
	}
}

func TestPackageMatchingSkipsClients(t *testing.T) {
	for name, want := range map[string]string{
		"postgresql-16": "postgres", "postgresql16-server": "postgres", "postgresql": "postgres",
		"postgresql-client-16": "", "postgresql-common": "", "libpq5": "",
		"redis-server": "redis", "redis-tools": "", "nginx-core": "nginx", "mysql-client": "",
	} {
		if got := kindByPackage(name); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
}

func TestImageName(t *testing.T) {
	for ref, want := range map[string]string{
		"postgres:16":                       "postgres",
		"docker.io/bitnami/postgresql:15.4": "postgresql",
		"registry:5000/team/redis":          "redis",
		"rabbitmq@sha256:abc":               "rabbitmq",
	} {
		if got, _ := imageName(ref); got != want {
			t.Errorf("%s: %q, want %q", ref, got, want)
		}
	}
}

// Only names, images, state and published TCP ports are kept from the
// runtime's list. Labels never make it into a Container.
func TestDecodeContainersKeepsNoLabels(t *testing.T) {
	cs, err := decodeContainers(strings.NewReader(`[{"Names":["/db"],"Image":"postgres:16","State":"running",
		"Labels":{"secret":"hunter2"},
		"Ports":[{"IP":"127.0.0.1","PrivatePort":5432,"PublicPort":15432,"Type":"tcp"},
		         {"PrivatePort":5432,"Type":"tcp"},{"PrivatePort":53,"PublicPort":53,"Type":"udp"}]}]`))
	if err != nil || len(cs) != 1 {
		t.Fatalf("%v %+v", err, cs)
	}
	c := cs[0]
	if c.Name != "db" || len(c.Ports) != 1 || c.Ports[0] != (ListenAddr{"127.0.0.1", 15432}) {
		t.Errorf("container = %+v", c)
	}
}
