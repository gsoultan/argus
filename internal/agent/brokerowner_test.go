//go:build !windows

package agent

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/gsoultan/argus/internal/tunnel"
)

// A new vhost with its app's user, against a real RabbitMQ: the user signs
// in, has everything on the vhost, the password is in no statement, and a
// user that is there is refused as new and taken as existing.
func TestRabbitCreateWithAppUser(t *testing.T) {
	if os.Getenv("ARGUS_TEST_RABBITMQ_PORT") == "" {
		t.Skip("ARGUS_TEST_RABBITMQ_PORT is not set")
	}
	ctx := t.Context()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	vhost, user, pw := "/pay-"+suffix, "pay-app-"+suffix, "a-long-password-"+suffix
	t.Cleanup(func() {
		for _, v := range []string{vhost, vhost + "-2"} {
			_, _ = runRabbitmqctl(context.Background(), []string{"delete_vhost", v}, "")
		}
		_, _ = runRabbitmqctl(context.Background(), []string{"delete_user", user}, "")
	})
	c := tunnel.GrantCreate{Op: tunnel.CreateOpCreate, Engine: tunnel.EngineRabbitMQ, Host: "127.0.0.1", Port: 15672, Name: vhost,
		Owner: &tunnel.CreateOwner{Kind: tunnel.OwnerNew, Name: user}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	res := createRabbit(ctx, runRabbitmqctl, c, pw)
	if !res.OK || res.AppPort != 5672 {
		t.Fatalf("%+v", res)
	}
	all := strings.Join(res.Statements, "\n")
	if strings.Contains(all, pw) || !strings.Contains(all, "rabbitmqctl add_user "+user+" "+tunnel.PasswordRedacted) ||
		!strings.Contains(all, `set_permissions -p `+vhost+` `+user+` ".*" ".*" ".*"`) {
		t.Fatalf("statements: %s", all)
	}
	out, err := runRabbitmqctl(ctx, []string{"list_user_permissions", user, "--formatter", "json"}, "")
	if err != nil || !strings.Contains(string(out), vhost) {
		t.Fatalf("permissions: %s %v", out, err)
	}
	if again := createRabbit(ctx, runRabbitmqctl, tunnel.GrantCreate{Op: tunnel.CreateOpCreate, Engine: tunnel.EngineRabbitMQ,
		Host: "127.0.0.1", Port: 15672, Name: vhost + "-2", Owner: &tunnel.CreateOwner{Kind: tunnel.OwnerNew, Name: user}}, pw); again.OK ||
		!strings.Contains(again.Error, "already") {
		t.Fatalf("a second new user of that name: %+v", again)
	}
	if ex := createRabbit(ctx, runRabbitmqctl, tunnel.GrantCreate{Op: tunnel.CreateOpCreate, Engine: tunnel.EngineRabbitMQ,
		Host: "127.0.0.1", Port: 15672, Name: vhost + "-2", Rabbit: &tunnel.RabbitCreate{},
		Owner: &tunnel.CreateOwner{Kind: tunnel.OwnerExisting, Name: user}}, ""); !ex.OK {
		t.Fatalf("an existing user: %+v", ex)
	}
	opts := createRabbit(ctx, runRabbitmqctl, tunnel.GrantCreate{Op: tunnel.CreateOpOptions, Engine: tunnel.EngineRabbitMQ}, "")
	found := false
	for _, u := range opts.Options.Users {
		found = found || u.Name == user
		if u.Name == "guest" || tunnel.IsStoreUser(u.Name) {
			t.Fatalf("offered %s", u.Name)
		}
	}
	if !found {
		t.Fatalf("the user is not offered: %+v", opts.Options.Users)
	}
	if err := (&tunnel.GrantCreate{Op: tunnel.CreateOpSetOwner, Engine: tunnel.EngineRabbitMQ, Host: "127.0.0.1", Port: 15672,
		Name: vhost, Owner: &tunnel.CreateOwner{Kind: tunnel.OwnerExisting, Name: user}}).Validate(); err == nil {
		t.Fatal("a vhost's owner was changed")
	}
}

// A new topic with its app's user, against a real Kafka: the user signs in,
// produces and reads the topic with a group starting with its name.
func TestKafkaCreateWithAppUser(t *testing.T) {
	k := startKafka(t)
	ctx := t.Context()
	port, _ := strconv.Atoi(os.Getenv("ARGUS_TEST_KAFKA_PORT"))
	topic, user, pw := k.scope+"refunds", "refunds-app-"+strconv.FormatInt(time.Now().UnixNano()%1e9, 36), "a-long-password-123"
	t.Cleanup(func() {
		_, _ = k.adm.AlterUserSCRAMs(context.Background(), []kadm.DeleteSCRAM{{User: user, Mechanism: kadm.ScramSha512}}, nil)
		_, _ = k.adm.DeleteACLs(context.Background(), kadm.NewACLs().Allow("User:"+user).AnyResource().ResourcePatternType(kadm.ACLPatternAny).Operations())
	})
	c := tunnel.GrantCreate{Op: tunnel.CreateOpCreate, Engine: tunnel.EngineKafka, Host: "127.0.0.1", Port: port, ConfigPath: k.admin,
		Name: topic, Kafka: &tunnel.KafkaCreate{Partitions: 1}, Owner: &tunnel.CreateOwner{Kind: tunnel.OwnerNew, Name: user}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	res := createKafka(ctx, c, pw)
	if !res.OK || res.AppPort != port {
		t.Fatalf("%+v", res)
	}
	all := strings.Join(res.Statements, "\n")
	if strings.Contains(all, pw) || !strings.HasPrefix(all, "AlterUserSCRAMs upsert "+user) || !strings.Contains(all, "group PREFIXED "+topic) {
		t.Fatalf("statements: %s", all)
	}
	cl, err := kafkaClient("127.0.0.1:"+strconv.Itoa(port), user, pw, "SCRAM-SHA-512", 10*time.Second,
		kgo.DisableIdempotentWrite(), kgo.DefaultProduceTopic(topic))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	if err := cl.ProduceSync(ctx, &kgo.Record{Value: []byte("x")}).FirstErr(); err != nil {
		t.Fatalf("the app user cannot produce: %v", err)
	}
	if _, err := kadm.NewClient(cl).FetchOffsets(ctx, topic+"-workers"); err != nil {
		t.Fatalf("the app user cannot use its group: %v", err)
	}
	again := createKafka(ctx, tunnel.GrantCreate{Op: tunnel.CreateOpCreate, Engine: tunnel.EngineKafka, Host: "127.0.0.1", Port: port,
		ConfigPath: k.admin, Name: topic + "2", Kafka: &tunnel.KafkaCreate{Partitions: 1},
		Owner: &tunnel.CreateOwner{Kind: tunnel.OwnerNew, Name: user}}, pw)
	if again.OK || !strings.Contains(again.Error, "already") {
		t.Fatalf("a second new user of that name: %+v", again)
	}
	a, _ := readKafkaAdmin(k.admin)
	self := createKafka(ctx, tunnel.GrantCreate{Op: tunnel.CreateOpCreate, Engine: tunnel.EngineKafka, Host: "127.0.0.1", Port: port,
		ConfigPath: k.admin, Name: topic + "3", Kafka: &tunnel.KafkaCreate{Partitions: 1},
		Owner: &tunnel.CreateOwner{Kind: tunnel.OwnerExisting, Name: a.User}}, "")
	if self.OK {
		t.Fatalf("the agent's own user was given a topic: %+v", self)
	}
}
