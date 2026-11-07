//go:build !windows

package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Against a real Kafka: ARGUS_TEST_KAFKA_PORT names a SASL_PLAINTEXT listener on
127.0.0.1 with an authorizer on, and ARGUS_TEST_KAFKA_ADMIN a file (mode 600)
with user = and password = for a SCRAM-SHA-512 super user. Without them these
tests skip; the file reading, the ACLs a level gets and the checks before any
connection are tested regardless.
*/

type kafkaServer struct {
	port  int
	admin string
	scope string
	adm   *kadm.Client
}

func startKafka(t *testing.T) kafkaServer {
	t.Helper()
	p := os.Getenv("ARGUS_TEST_KAFKA_PORT")
	file := os.Getenv("ARGUS_TEST_KAFKA_ADMIN")
	if p == "" || file == "" {
		t.Skip("ARGUS_TEST_KAFKA_PORT and ARGUS_TEST_KAFKA_ADMIN are not set")
	}
	port, _ := strconv.Atoi(p)
	a, err := readKafkaAdmin(file)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := kafkaClient("127.0.0.1:"+p, a.User, a.Password, a.Mechanism, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	adm := kadm.NewClient(cl)
	scope := "argus-t-" + strconv.FormatInt(time.Now().UnixNano()%1e9, 36) + "."
	t.Cleanup(func() {
		ctx := context.Background()
		if td, err := adm.ListTopics(ctx); err == nil {
			var mine []string
			for _, n := range td.Names() {
				if strings.HasPrefix(n, scope) {
					mine = append(mine, n)
				}
			}
			if len(mine) > 0 {
				_, _ = adm.DeleteTopics(ctx, mine...)
			}
		}
		cl.Close()
	})
	return kafkaServer{port: port, admin: file, scope: scope, adm: adm}
}

func (k kafkaServer) topic(t *testing.T, name string, parts int32) {
	t.Helper()
	if _, err := k.adm.CreateTopic(context.Background(), parts, 1, nil, name); err != nil {
		t.Fatal(err)
	}
}

func (k kafkaServer) users(t *testing.T) []string {
	t.Helper()
	users, err := k.adm.DescribeUserSCRAMs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for u := range users {
		out = append(out, u)
	}
	return out
}

func openKafka(t *testing.T, k kafkaServer, svc TunnelService, level, scope string) *storeHarness {
	t.Helper()
	h, err := openStoreSession(t, svc, tunnel.KafkaService, tunnel.GrantDB{ID: "k1", Engine: tunnel.EngineKafka,
		Name: scope, Host: "127.0.0.1", Port: k.port, Level: level, ConfigPath: k.admin, MaxRows: 50}, t.TempDir())
	if err != nil {
		t.Fatalf("kafka session refused: %v", err)
	}
	return h
}

func dataOf[T any](t *testing.T, a tunnel.StoreAnswer) T {
	t.Helper()
	var v T
	b, _ := json.Marshal(a.Data)
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("answer data %s: %v", b, err)
	}
	return v
}

func TestKafkaLevelsAreACLs(t *testing.T) {
	k := startKafka(t)
	orders, other := k.scope+"orders", "zz-"+k.scope+"other"
	k.topic(t, orders, 2)
	k.topic(t, other, 1)
	t.Cleanup(func() { _, _ = k.adm.DeleteTopics(context.Background(), other) })
	svc := Kafka(StoreConfig{Verifier: &tunnel.GrantVerifier{Host: "cache-01"}, RecordDir: t.TempDir()})

	read := openKafka(t, k, svc, tunnel.LevelRead, k.scope)
	write := openKafka(t, k, svc, tunnel.LevelWrite, k.scope)
	owner := openKafka(t, k, svc, tunnel.LevelOwner, k.scope)

	topics := dataOf[[]KafkaTopic](t, read.ask(tunnel.StoreRequest{Op: "topics"}))
	if len(topics) != 1 || topics[0].Name != orders || topics[0].Partitions != 2 {
		t.Fatalf("read sees %+v, want only %s", topics, orders)
	}
	if a := read.ask(tunnel.StoreRequest{Op: "publish", Name: orders, Payload: "x"}); a.Type != "error" {
		t.Fatalf("read published: %+v", a)
	}
	if a := write.ask(tunnel.StoreRequest{Op: "peek", Name: other}); a.Type != "error" || !strings.Contains(a.Error, "not one of") {
		t.Fatalf("a topic outside the scope: %+v", a)
	}

	pub := write.ask(tunnel.StoreRequest{Op: "publish", Name: orders, RoutingKey: "o-1", Payload: `{"total":12}`,
		Headers: map[string]string{"source": "argus"}})
	if pub.Type != "result" {
		t.Fatalf("publish: %+v", pub)
	}
	if !write.recorded("stmt", `publish `+orders+` key="o-1" header "source"="argus" {"total":12}`) {
		t.Fatalf("publish was not recorded: %+v", write.rec)
	}
	peek := read.ask(tunnel.StoreRequest{Op: "peek", Name: orders, Count: 5})
	msgs := dataOf[[]KafkaMessage](t, peek)
	if peek.Type != "result" || len(msgs) != 1 || msgs[0].Key != "o-1" || msgs[0].Value != `{"total":12}` || msgs[0].Headers["source"] != "argus" {
		t.Fatalf("peek: %+v %+v", peek, msgs)
	}
	if !read.recorded("stmt", "peek "+orders+" last 5") {
		t.Fatalf("peek was not recorded: %+v", read.rec)
	}

	if a := write.ask(tunnel.StoreRequest{Op: "create_topic", Name: k.scope + "new"}); a.Type != "error" {
		t.Fatalf("write created a topic: %+v", a)
	}
	if a := owner.ask(tunnel.StoreRequest{Op: "create_topic", Name: k.scope + "new", Count: 3}); a.Type != "result" {
		t.Fatalf("owner create: %+v", a)
	}
	if a := owner.ask(tunnel.StoreRequest{Op: "create_topic", Name: "elsewhere"}); a.Type != "error" {
		t.Fatalf("owner created a topic outside the scope: %+v", a)
	}
	if a := owner.ask(tunnel.StoreRequest{Op: "delete_topic", Name: k.scope + "new"}); !a.NeedsConfirm {
		t.Fatalf("delete without confirm: %+v", a)
	}
	if a := owner.ask(tunnel.StoreRequest{Op: "delete_topic", Name: k.scope + "new", Confirm: "cache-01"}); a.Type != "result" {
		t.Fatalf("delete: %+v", a)
	}

	detail := dataOf[map[string]any](t, owner.ask(tunnel.StoreRequest{Op: "topic", Name: orders}))
	if parts, _ := detail["partitions"].([]any); len(parts) != 2 {
		t.Fatalf("topic detail: %+v", detail)
	}
	if a := owner.ask(tunnel.StoreRequest{Op: "overview"}); a.Type != "result" || a.Total != 1 {
		t.Fatalf("overview: %+v", a)
	}

	for _, h := range []*storeHarness{read, write, owner} {
		h.close()
	}
	for _, u := range k.users(t) {
		if tunnel.IsStoreUser(u) {
			t.Fatalf("user %s is left after its sessions closed", u)
		}
	}
}

func TestKafkaRefusesBeforeConnecting(t *testing.T) {
	svc := Kafka(StoreConfig{Verifier: &tunnel.GrantVerifier{Host: "cache-01"}, RecordDir: t.TempDir()})
	_, err := openStoreSession(t, svc, tunnel.KafkaService, tunnel.GrantDB{ID: "k1", Engine: tunnel.EngineKafka,
		Name: "*", Host: "kafka.internal", Port: 9092, Level: tunnel.LevelRead}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "must be on this server") {
		t.Fatalf("a remote host: %v", err)
	}
	_, err = openStoreSession(t, svc, tunnel.KafkaService, tunnel.GrantDB{ID: "k1", Engine: tunnel.EngineKafka,
		Name: "orders/*", Host: "127.0.0.1", Port: 9092, Level: tunnel.LevelRead}, t.TempDir())
	if err == nil {
		t.Fatal("a scope Kafka cannot name was opened")
	}
	_, err = openStoreSession(t, svc, tunnel.KafkaService, tunnel.GrantDB{ID: "k1", Engine: tunnel.EngineKafka,
		Name: "*", Host: "127.0.0.1", Port: 9092, Level: tunnel.LevelRead,
		ConfigPath: filepath.Join(t.TempDir(), "missing.conf")}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("a missing admin file: %v", err)
	}
}

func TestReadKafkaAdmin(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "kafka.conf")
	if err := os.WriteFile(file, []byte("# Argus's Kafka admin\nuser = argus-admin\npassword: \"s3cret = yes\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := readKafkaAdmin(file)
	if err != nil || a.User != "argus-admin" || a.Password != "s3cret = yes" || a.Mechanism != "SCRAM-SHA-512" {
		t.Fatalf("read: %+v %v", a, err)
	}
	if err := os.WriteFile(file, []byte("user=a\npassword=b\nmechanism=PLAIN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readKafkaAdmin(file); err == nil || !strings.Contains(err.Error(), "SCRAM") {
		t.Fatalf("a PLAIN admin: %v", err)
	}
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readKafkaAdmin(file); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("a file others can read: %v", err)
	}
	if _, err := readKafkaAdmin("/etc/passwd"); err == nil {
		t.Fatal("a file that is not a .conf was read")
	}
}

func TestKafkaGrants(t *testing.T) {
	s := &kafkaSession{user: "argus-dev-read", db: tunnel.GrantDB{Name: "orders.", Level: tunnel.LevelRead}}
	b := s.grants()
	if err := b.ValidateCreate(); err != nil {
		t.Fatal(err)
	}
	s.db.Name = "*"
	if err := s.grants().ValidateCreate(); err != nil {
		t.Fatal(err)
	}
	if err := s.groupGrants().ValidateCreate(); err != nil {
		t.Fatal(err)
	}
	if got := len(tunnel.KafkaTopicOps(tunnel.LevelOwner)); got != 8 {
		t.Fatalf("owner holds %d operations", got)
	}
}

func TestKafkaText(t *testing.T) {
	if s, b64, cut := kafkaText([]byte("héllo"), 100); s != "héllo" || b64 || cut {
		t.Fatalf("text: %q %v %v", s, b64, cut)
	}
	if s, b64, _ := kafkaText([]byte{0xff, 0x00, 0x01}, 100); s != "/wAB" || !b64 {
		t.Fatalf("binary: %q %v", s, b64)
	}
	if s, _, cut := kafkaText([]byte("abcdef"), 3); s != "abc" || !cut {
		t.Fatalf("cut: %q %v", s, cut)
	}
}
