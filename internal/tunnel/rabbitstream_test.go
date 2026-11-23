package tunnel

import (
	"strings"
	"testing"
)

func TestStreamReadRules(t *testing.T) {
	for _, r := range []*RabbitStreamRead{{From: "first"}, {From: "last"}, {From: "offset", Offset: 0}, {From: "time", At: 1}} {
		if why := ValidStreamRead(r); why != "" {
			t.Errorf("%+v: %s", r, why)
		}
	}
	for _, r := range []*RabbitStreamRead{nil, {From: "next"}, {From: "offset", Offset: -1}, {From: "time"}} {
		if ValidStreamRead(r) == "" {
			t.Errorf("%+v passed", r)
		}
	}
	if got := StreamReadText("orders.events", 20, &RabbitStreamRead{From: "offset", Offset: 48211}); got != `READ STREAM "orders.events" from offset 48211 count 20` {
		t.Errorf("text: %s", got)
	}
	if !RabbitOpAllowed(RabbitOpStreamRead, LevelRead) || RabbitOpAllowed(RabbitOpDeclareSuperStream, LevelWrite) ||
		!RabbitOpAllowed(RabbitOpDeleteSuperStream, LevelOwner) || !RabbitNeedsConfirm(RabbitOpDeleteSuperStream) {
		t.Error("levels")
	}
}

func TestSuperStreamRules(t *testing.T) {
	ok := []struct {
		name string
		s    RabbitSuperStream
	}{
		{"payments", RabbitSuperStream{Partitions: 4, MaxAgeS: 86400}},
		{"regions", RabbitSuperStream{Keys: []string{"eu", "us"}}},
	}
	for _, c := range ok {
		if why := ValidSuperStream(c.name, &c.s); why != "" {
			t.Errorf("%s: %s", c.name, why)
		}
	}
	bad := []struct {
		name string
		s    *RabbitSuperStream
	}{
		{"payments", nil},
		{"payments", &RabbitSuperStream{}},
		{"payments", &RabbitSuperStream{Partitions: 65}},
		{"amq.x", &RabbitSuperStream{Partitions: 2}},
		{"a b", &RabbitSuperStream{Partitions: 2}},
		{"r", &RabbitSuperStream{Keys: []string{"eu", "eu"}}},
		{"r", &RabbitSuperStream{Keys: []string{"e u"}}},
		{"r", &RabbitSuperStream{Keys: []string{"eu"}, Partitions: 3}},
		{"r", &RabbitSuperStream{Partitions: 2, MaxAgeS: -1}},
	}
	for _, c := range bad {
		if ValidSuperStream(c.name, c.s) == "" {
			t.Errorf("%s %+v passed", c.name, c.s)
		}
	}
	text := SuperStreamText("payments", &RabbitSuperStream{Partitions: 2, MaxAgeS: 60})
	for _, want := range []string{`DECLARE SUPER STREAM "payments" partitions 2`, `stream "payments-1" x-max-age 60s bound with key "1" order 1`} {
		if !strings.Contains(text, want) {
			t.Errorf("text has no %q:\n%s", want, text)
		}
	}
}
