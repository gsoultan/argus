//go:build !windows

package agent

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

func decodeAs[T any](t *testing.T, a tunnel.StoreAnswer) T {
	t.Helper()
	if a.Type != "result" {
		t.Fatalf("answer: %+v", a)
	}
	b, _ := json.Marshal(a.Data)
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// Against a real RabbitMQ (ARGUS_TEST_RABBITMQ_PORT): a stream is read from
// its start, its end, an offset and a time at Read, as the person's user,
// without anyone's place moving; super streams are made, listed and deleted
// at Owner.
func TestRabbitStreamsAgainstRealBroker(t *testing.T) {
	port, _ := strconv.Atoi(os.Getenv("ARGUS_TEST_RABBITMQ_PORT"))
	if port == 0 {
		t.Skip("ARGUS_TEST_RABBITMQ_PORT is not set")
	}
	dir := t.TempDir()
	svc := RabbitMQ(RabbitConfig{StoreConfig: StoreConfig{Verifier: &tunnel.GrantVerifier{Host: "cache-01"}, RecordDir: dir}})
	vhost := "streams-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := runRabbitmqctl(t.Context(), []string{"add_vhost", vhost}, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = runRabbitmqctl(t.Context(), []string{"delete_vhost", vhost}, "") })
	open := func(level string) *storeHarness {
		h, err := openStoreSession(t, svc, tunnel.RabbitMQService, tunnel.GrantDB{ID: "q1", Engine: tunnel.EngineRabbitMQ,
			Name: vhost, Host: "127.0.0.1", Port: port, Level: level}, dir)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	owner := open(tunnel.LevelOwner)
	defer owner.close()
	if a := owner.ask(tunnel.StoreRequest{Op: "declare_queue", Name: "events", Queue: &tunnel.RabbitQueue{Type: "stream"}}); a.Type != "result" {
		t.Fatalf("declare: %+v", a)
	}
	start := time.Now()
	for i := range 30 {
		if a := owner.ask(tunnel.StoreRequest{Op: "publish", RoutingKey: "events", Payload: `{"n":` + strconv.Itoa(i) + `}`,
			Properties: map[string]string{"content_type": "application/json"}, Headers: map[string]string{"source": "test"}}); a.Type != "result" {
			t.Fatalf("publish: %+v", a)
		}
	}

	reader := open(tunnel.LevelRead)
	defer reader.close()
	read := func(count int, r tunnel.RabbitStreamRead) StreamReadResult {
		return decodeAs[StreamReadResult](t, reader.ask(tunnel.StoreRequest{Op: tunnel.RabbitOpStreamRead, Name: "events", Count: count, Stream: &r}))
	}
	r := read(10, tunnel.RabbitStreamRead{From: "first"})
	if len(r.Messages) != 10 || r.Messages[0].Offset != r.First || r.Last-r.First != 29 {
		t.Fatalf("first: %+v", r)
	}
	m := r.Messages[0]
	if m.Body != `{"n":0}` || m.Encoding != "string" || m.Properties["content_type"] != "application/json" || m.Headers["source"] != "test" {
		t.Fatalf("message: %+v", m)
	}
	if r := read(5, tunnel.RabbitStreamRead{From: "last"}); len(r.Messages) != 5 || r.Messages[4].Offset != r.Last || r.Messages[4].Body != `{"n":29}` {
		t.Fatalf("last: %+v", r)
	}
	if r := read(3, tunnel.RabbitStreamRead{From: "offset", Offset: 12}); len(r.Messages) != 3 || r.Messages[0].Offset != 12 || r.Messages[2].Offset != 14 {
		t.Fatalf("offset: %+v", r)
	}
	if r := read(10, tunnel.RabbitStreamRead{From: "offset", Offset: 28}); len(r.Messages) != 2 {
		t.Fatalf("near the end: %+v", r)
	}
	if r := read(10, tunnel.RabbitStreamRead{From: "offset", Offset: 500}); len(r.Messages) != 0 {
		t.Fatalf("past the end: %+v", r)
	}
	if r := read(50, tunnel.RabbitStreamRead{From: "time", At: start.Add(-time.Minute).UnixMilli()}); len(r.Messages) != 30 {
		t.Fatalf("from a time: %d", len(r.Messages))
	}
	if !reader.recordedHas("stmt", `READ STREAM "events" from offset 12 count 3`) {
		t.Fatal("the read is not recorded")
	}
	// Still all there: reading takes nothing.
	if r := read(100, tunnel.RabbitStreamRead{From: "first"}); len(r.Messages) != 30 {
		t.Fatalf("after reading: %d", len(r.Messages))
	}
	if a := reader.ask(tunnel.StoreRequest{Op: tunnel.RabbitOpStreamRead, Name: "events", Stream: &tunnel.RabbitStreamRead{From: "later"}}); a.Type != "error" {
		t.Fatalf("a bad start: %+v", a)
	}

	// A queue is not read this way.
	if a := owner.ask(tunnel.StoreRequest{Op: "declare_queue", Name: "jobs", Durable: true, Queue: &tunnel.RabbitQueue{Type: "quorum"}}); a.Type != "result" {
		t.Fatalf("declare: %+v", a)
	}
	if a := reader.ask(tunnel.StoreRequest{Op: tunnel.RabbitOpStreamRead, Name: "jobs", Stream: &tunnel.RabbitStreamRead{From: "first"}}); a.Type != "error" || !strings.Contains(a.Error, "not a stream") {
		t.Fatalf("a queue read as a stream: %+v", a)
	}

	readers := decodeAs[StreamReaders](t, reader.ask(tunnel.StoreRequest{Op: tunnel.RabbitOpStreamReaders, Name: "events"}))
	if len(readers.Readers) != 0 {
		t.Fatalf("readers: %+v", readers)
	}

	// Super streams: Owner makes them, Read only lists.
	ss := tunnel.StoreRequest{Op: tunnel.RabbitOpDeclareSuperStream, Name: "payments", SuperStream: &tunnel.RabbitSuperStream{Partitions: 3, MaxAgeS: 86400}}
	if a := reader.ask(ss); a.Type != "error" {
		t.Fatalf("Read made a super stream: %+v", a)
	}
	if a := owner.ask(ss); a.Type != "result" {
		t.Fatalf("super stream: %+v", a)
	}
	if a := owner.ask(ss); a.Type != "error" || !strings.Contains(a.Error, "already") {
		t.Fatalf("made twice: %+v", a)
	}
	if a := owner.ask(tunnel.StoreRequest{Op: tunnel.RabbitOpDeclareSuperStream, Name: "regions",
		SuperStream: &tunnel.RabbitSuperStream{Keys: []string{"eu", "us"}}}); a.Type != "result" {
		t.Fatalf("super stream by keys: %+v", a)
	}
	if a := owner.ask(tunnel.StoreRequest{Op: "publish", Exchange: "regions", RoutingKey: "us", Payload: "hello"}); a.Type != "result" {
		t.Fatalf("publish to a super stream: %+v", a)
	}
	list := decodeAs[[]SuperStream](t, reader.ask(tunnel.StoreRequest{Op: tunnel.RabbitOpSuperStreams}))
	got := map[string]SuperStream{}
	for _, s := range list {
		got[s.Name] = s
	}
	if p := got["payments"].Partitions; len(p) != 3 || p[0].Stream != "payments-0" || p[2].RoutingKey != "2" || p[2].Order != 2 {
		t.Fatalf("payments: %+v", got["payments"])
	}
	if p := got["regions"].Partitions; len(p) != 2 || p[1].Stream != "regions-us" {
		t.Fatalf("regions: %+v", got["regions"])
	}
	if r := decodeAs[StreamReadResult](t, reader.ask(tunnel.StoreRequest{Op: tunnel.RabbitOpStreamRead, Name: "regions-us",
		Stream: &tunnel.RabbitStreamRead{From: "first"}})); len(r.Messages) != 1 || r.Messages[0].Body != "hello" {
		t.Fatalf("a partition: %+v", r)
	}
	del := tunnel.StoreRequest{Op: tunnel.RabbitOpDeleteSuperStream, Name: "payments"}
	if a := owner.ask(del); a.Type != "error" || !a.NeedsConfirm {
		t.Fatalf("deleted without the server's name: %+v", a)
	}
	del.Confirm = "cache-01"
	if a := owner.ask(del); a.Type != "result" {
		t.Fatalf("delete: %+v", a)
	}
	list = decodeAs[[]SuperStream](t, reader.ask(tunnel.StoreRequest{Op: tunnel.RabbitOpSuperStreams}))
	if len(list) != 1 || list[0].Name != "regions" {
		t.Fatalf("after delete: %+v", list)
	}
	if a := owner.ask(tunnel.StoreRequest{Op: "queue", Name: "payments-1"}); a.Type != "error" {
		t.Fatalf("a partition stayed: %+v", a)
	}
}
