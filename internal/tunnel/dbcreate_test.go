package tunnel

import (
	"strings"
	"testing"
)

// Every engine's statements are built from checked fields only, quoted where
// a name needs it; what CreatePlan says is what the agent runs.
func TestCreatePlanPerEngine(t *testing.T) {
	cases := []struct {
		c    GrantCreate
		want []string
	}{
		{GrantCreate{Op: "create", Engine: EnginePostgres, Host: "/var/run/postgresql", Port: 5432, Name: "invoices",
			Postgres: &PGCreate{Encoding: "UTF8", Locale: "en_US.UTF-8", Extensions: []string{"pgcrypto", "uuid-ossp"}}},
			[]string{"CREATE DATABASE invoices OWNER argus_owner_invoices TEMPLATE template0 ENCODING 'UTF8' LC_COLLATE 'en_US.UTF-8' LC_CTYPE 'en_US.UTF-8'",
				"CREATE EXTENSION IF NOT EXISTS pgcrypto", `CREATE EXTENSION IF NOT EXISTS "uuid-ossp"`}},
		{GrantCreate{Op: "create", Engine: EnginePostgres, Host: "/var/run/postgresql", Port: 5432, Name: "Order Book",
			Postgres: &PGCreate{Owner: "app"}},
			[]string{`CREATE DATABASE "Order Book" OWNER app TEMPLATE template0`}},
		{GrantCreate{Op: "create", Engine: EngineMySQL, Host: "127.0.0.1", Port: 3306, Name: "shop_returns",
			MySQL: &MySQLCreate{Charset: "UTF8MB4", Collation: "utf8mb4_0900_ai_ci"}},
			[]string{"CREATE DATABASE `shop_returns` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"}},
		{GrantCreate{Op: "create", Engine: EngineMongoDB, Host: "127.0.0.1", Port: 27017, Name: "telemetry",
			Mongo: &MongoCreate{Collection: "events", Kind: "timeseries", TimeField: "ts", ExpireAfterS: 2592000}},
			[]string{"use telemetry", `db.createCollection("events", { timeseries: { timeField: "ts" }, expireAfterSeconds: 2592000 })`}},
		{GrantCreate{Op: "create", Engine: EngineRabbitMQ, Host: "127.0.0.1", Port: 15672, Name: "/payments",
			Rabbit: &RabbitCreate{Description: "it's payments", QueueType: "quorum", MaxConnections: 10}},
			[]string{`rabbitmqctl add_vhost /payments --description 'it'\''s payments' --default-queue-type quorum`,
				`rabbitmqctl set_vhost_limits -p /payments '{"max-connections":10}'`}},
		{GrantCreate{Op: "create", Engine: EngineKafka, Host: "127.0.0.1", Port: 9092, Name: "orders.refunds",
			Kafka: &KafkaCreate{Partitions: 6, Replicas: 3, RetentionMS: 604800000, Cleanup: "delete"}},
			[]string{"CreateTopics orders.refunds partitions=6 replication.factor=3 retention.ms=604800000 cleanup.policy=delete"}},
	}
	for _, tc := range cases {
		if err := tc.c.Validate(); err != nil {
			t.Fatalf("%s %s: %v", tc.c.Engine, tc.c.Name, err)
		}
		got := CreatePlan(tc.c)
		if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
			t.Errorf("%s:\n got %q\nwant %q", tc.c.Engine, got, tc.want)
		}
	}
}

func TestCreateValidateRefuses(t *testing.T) {
	pg := func(name string, p *PGCreate) GrantCreate {
		return GrantCreate{Op: "create", Engine: EnginePostgres, Host: "/var/run/postgresql", Port: 5432, Name: name, Postgres: p}
	}
	bad := []GrantCreate{
		pg(`a"b`, nil), pg("", nil), pg("postgres", nil), pg("template1", nil), pg("argus_test_x", nil),
		pg("x", &PGCreate{Owner: "pg_execute_server_program"}),
		pg("x", &PGCreate{Owner: "a b"}),
		pg("x", &PGCreate{Encoding: "UTF8'; DROP"}),
		pg("x", &PGCreate{Locale: "en US"}),
		pg("x", &PGCreate{Extensions: []string{"pgcrypto; DROP TABLE x"}}),
		pg("x", &PGCreate{Extensions: []string{"pgcrypto", "pgcrypto"}}),
		{Op: "create", Engine: EnginePostgres, Host: "db.internal", Port: 5432, Name: "x"},
		{Op: "create", Engine: EngineMySQL, Host: "127.0.0.1", Port: 3306, Name: "mysql"},
		{Op: "create", Engine: EngineMySQL, Host: "127.0.0.1", Port: 3306, Name: "a.b"},
		{Op: "create", Engine: EngineMySQL, Host: "127.0.0.1", Port: 3306, Name: "x", MySQL: &MySQLCreate{Charset: "utf8mb4", Collation: "latin1_swedish_ci"}},
		{Op: "create", Engine: EngineMySQL, Host: "127.0.0.1", Port: 3306, Name: "x", MySQL: &MySQLCreate{Charset: "utf8mb4 COLLATE x"}},
		{Op: "create", Engine: EngineMongoDB, Host: "127.0.0.1", Port: 27017, Name: "x"},
		{Op: "create", Engine: EngineMongoDB, Host: "127.0.0.1", Port: 27017, Name: "admin", Mongo: &MongoCreate{Collection: "c"}},
		{Op: "create", Engine: EngineMongoDB, Host: "127.0.0.1", Port: 27017, Name: "x", Mongo: &MongoCreate{Collection: "system.users"}},
		{Op: "create", Engine: EngineMongoDB, Host: "127.0.0.1", Port: 27017, Name: "x", Mongo: &MongoCreate{Collection: "c", Kind: "timeseries"}},
		{Op: "create", Engine: EngineMongoDB, Host: "127.0.0.1", Port: 27017, Name: "x", Mongo: &MongoCreate{Collection: "c", Kind: "capped", CappedBytes: 10}},
		{Op: "create", Engine: EngineRabbitMQ, Host: "127.0.0.1", Port: 15672, Name: "x", Rabbit: &RabbitCreate{QueueType: "lazy"}},
		{Op: "create", Engine: EngineRabbitMQ, Host: "127.0.0.1", Port: 15672, Name: "x", Rabbit: &RabbitCreate{Description: "a\nb"}},
		{Op: "create", Engine: EngineRabbitMQ, Host: "10.1.1.1", Port: 15672, Name: "x"},
		{Op: "create", Engine: EngineKafka, Host: "127.0.0.1", Port: 9092, Name: "__consumer_offsets"},
		{Op: "create", Engine: EngineKafka, Host: "127.0.0.1", Port: 9092, Name: "a b"},
		{Op: "create", Engine: EngineKafka, Host: "127.0.0.1", Port: 9092, Name: "x", Kafka: &KafkaCreate{Partitions: 5000}},
		{Op: "create", Engine: EngineKafka, Host: "127.0.0.1", Port: 9092, Name: "x", Kafka: &KafkaCreate{Cleanup: "compact,delete"}},
		{Op: "create", Engine: EngineRedis, Host: "127.0.0.1", Port: 6379, Name: "3"},
		{Op: "set-owner", Engine: EngineMySQL, Host: "127.0.0.1", Port: 3306, Name: "x", Postgres: &PGCreate{Owner: "app"}},
		{Op: "set-owner", Engine: EnginePostgres, Host: "/var/run/postgresql", Port: 5432, Name: "x"},
		{Op: "drop", Engine: EnginePostgres, Host: "/var/run/postgresql", Port: 5432, Name: "x"},
	}
	for _, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("accepted %s %q %+v", c.Engine, c.Name, c)
		}
	}
}

// The level role name is the one sessions grant; moving it into tunnel
// must not rename anyone's role.
func TestPGLevelRoleIsStable(t *testing.T) {
	for in, want := range map[string]string{
		"orders":   "argus_owner_orders",
		"Orders":   "argus_owner_orders_",
		"my-app":   "argus_owner_my_app_",
		"invoices": "argus_owner_invoices",
	} {
		got := PGLevelRole(LevelOwner, in)
		if !strings.HasPrefix(got, want) || len(got) > 63 {
			t.Errorf("%s: %s", in, got)
		}
	}
	if PGLevelRole(LevelOwner, "Orders") == PGLevelRole(LevelOwner, "orders") {
		t.Fatal("two databases share a role")
	}
}

func TestRabbitQueueArguments(t *testing.T) {
	a, durable, err := RabbitQueue{Type: "stream", MaxAgeS: 604800, MaxLengthBytes: 20 << 30, SegmentBytes: 500 << 20}.Arguments()
	if err != nil || !durable || a["x-queue-type"] != "stream" || a["x-max-age"] != "604800s" ||
		a["x-max-length-bytes"] != int64(20<<30) || a["x-stream-max-segment-size-bytes"] != int64(500<<20) {
		t.Fatalf("stream: %v %v %v", a, durable, err)
	}
	a, durable, err = RabbitQueue{Type: "quorum", DeliveryLimit: 5, DeadLetterExchange: "payments.dlx"}.Arguments()
	if err != nil || !durable || a["x-delivery-limit"] != 5 || a["x-dead-letter-exchange"] != "payments.dlx" {
		t.Fatalf("quorum: %v %v %v", a, durable, err)
	}
	a, durable, err = RabbitQueue{}.Arguments()
	if err != nil || durable || len(a) != 0 {
		t.Fatalf("classic: %v %v %v", a, durable, err)
	}
	for _, q := range []RabbitQueue{
		{Type: "stream", MessageTTLMS: 10}, {Type: "stream", DeliveryLimit: 2}, {Type: "classic", MaxAgeS: 5},
		{Type: "quorum", SegmentBytes: 1}, {Type: "lazy"}, {MaxLength: -1},
	} {
		if _, _, err := q.Arguments(); err == nil {
			t.Errorf("accepted %+v", q)
		}
	}
}
