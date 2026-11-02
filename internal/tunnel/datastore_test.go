package tunnel

import (
	"reflect"
	"testing"
)

func TestSplitRedisCommand(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
	}{
		{`GET foo`, []string{"GET", "foo"}},
		{`  SET  "a b"   'c d' `, []string{"SET", "a b", "c d"}},
		{`SET k "line\nnext\x41"`, []string{"SET", "k", "line\nnextA"}},
		{`SET k 'it\'s'`, []string{"SET", "k", "it's"}},
		{`SET k ""`, []string{"SET", "k", ""}},
	} {
		got, err := SplitRedisCommand(c.in)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: got %q, %v", c.in, got, err)
		}
	}
	for _, bad := range []string{`GET "foo`, `GET 'foo`, `GET "a"b`} {
		if _, err := SplitRedisCommand(bad); err == nil {
			t.Errorf("%q split without an error", bad)
		}
	}
}

// What is recorded must split back into exactly what ran.
func TestJoinRedisCommandRoundTrips(t *testing.T) {
	for _, args := range [][]string{
		{"SET", "user:1", "plain"},
		{"SET", "k", "two words"},
		{"SET", "k", "quote\"and\\slash"},
		{"SET", "k", "\x00\xffbin"},
		{"SET", "k", ""},
		{"SET", "k", "naïve ünïcode"},
	} {
		line := JoinRedisCommand(args)
		back, err := SplitRedisCommand(line)
		if err != nil || !reflect.DeepEqual(back, args) {
			t.Errorf("%q recorded as %s, read back as %q (%v)", args, line, back, err)
		}
	}
}

func TestRedisGates(t *testing.T) {
	if RedisRefusal([]string{"auth", "default", "x"}) == "" || RedisRefusal([]string{"MONITOR"}) == "" ||
		RedisRefusal([]string{"blpop", "q", "0"}) == "" || RedisRefusal([]string{"XREAD", "BLOCK", "0", "STREAMS", "s", "$"}) == "" {
		t.Fatal("a command that takes the connection away was not refused")
	}
	if RedisRefusal([]string{"GET", "k"}) != "" || RedisRefusal([]string{"XREAD", "STREAMS", "s", "0"}) != "" {
		t.Fatal("an ordinary command was refused")
	}
	for _, c := range [][]string{{"flushall"}, {"FLUSHDB", "ASYNC"}, {"config", "set", "maxmemory", "1"}, {"SHUTDOWN"}} {
		if !RedisNeedsConfirm(c) {
			t.Errorf("%q does not ask for a confirm", c)
		}
	}
	for _, c := range [][]string{{"CONFIG", "GET", "*"}, {"DEL", "k"}, {"SET", "k", "v"}} {
		if RedisNeedsConfirm(c) {
			t.Errorf("%q asks for a confirm", c)
		}
	}
}

func TestRabbitOps(t *testing.T) {
	cases := map[string][]string{
		LevelRead:  {"queues", "peek"},
		LevelWrite: {"publish"},
		LevelOwner: {"purge", "delete_queue"},
		LevelDBA:   {"close_connection"},
	}
	for level, ops := range cases {
		for _, op := range ops {
			if !RabbitOpAllowed(op, level) {
				t.Errorf("%s may not %s", level, op)
			}
		}
	}
	if RabbitOpAllowed("purge", LevelWrite) || RabbitOpAllowed("publish", LevelRead) ||
		RabbitOpAllowed("anything", LevelDBA) || RabbitOpAllowed("queues", "") {
		t.Fatal("a level may do more than it should")
	}
}
