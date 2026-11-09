package agent

import (
	"encoding/binary"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

func utmpxRec(size int, typ int16, user, line, host string, sec int64) []byte {
	r := make([]byte, size)
	copy(r[0:256], user)
	copy(r[260:292], line)
	binary.LittleEndian.PutUint16(r[296:298], uint16(typ))
	if size == utmpxRecord32 {
		binary.LittleEndian.PutUint32(r[300:304], uint32(sec))
		copy(r[308:564], host)
	} else {
		binary.LittleEndian.PutUint64(r[304:312], uint64(sec))
		copy(r[320:576], host)
	}
	return r
}

func TestParseUtmpx(t *testing.T) {
	for _, size := range []int{utmpxRecord32, utmpxRecord64} {
		var b []byte
		b = append(b, utmpxRec(size, 10, "utmpx-1.00", "", "", 0)...) // signature
		b = append(b, utmpxRec(size, utmpxUserProcess, "ana", "console", "", 1_700_000_000)...)
		b = append(b, utmpxRec(size, utmpxUserProcess, "ana", "ttys000", "", 1_700_000_100)...)
		b = append(b, utmpxRec(size, utmpxUserProcess, "bo", "ttys001", "10.0.0.7", 1_700_000_200)...)
		b = append(b, utmpxRec(size, 8, "gone", "ttys002", "10.0.0.8", 1)...) // dead process

		got, ok := parseUtmpx(b)
		if !ok || len(got) != 3 {
			t.Fatalf("size %d: %v %+v", size, ok, got)
		}
		if !got[0].Console || got[0].Account != "ana" || got[0].Remote || got[0].LogonAt == nil || got[0].LogonAt.Unix() != 1_700_000_000 {
			t.Fatalf("size %d console: %+v", size, got[0])
		}
		if got[1].Console {
			t.Fatalf("size %d: a terminal window is not the screen: %+v", size, got[1])
		}
		if !got[2].Remote || got[2].From != "10.0.0.7" || got[2].Console {
			t.Fatalf("size %d remote: %+v", size, got[2])
		}
		if c := consoleLogons(got); len(c) != 1 || c[0].Account != "ana" {
			t.Fatalf("size %d: asked %+v", size, c)
		}
	}
}

// A file in neither layout is not read as "nobody is here": a workstation
// would then open without asking.
func TestParseUtmpxUnknownLayout(t *testing.T) {
	if _, ok := parseUtmpx(make([]byte, 384*3)); ok {
		t.Fatal("a Linux utmp file read as utmpx")
	}
	junk := utmpxRec(utmpxRecord32, 99, "x", "", "", 0)
	if _, ok := parseUtmpx(junk); ok {
		t.Fatal("an unknown record type was accepted")
	}
	if got, ok := parseUtmpx(nil); !ok || len(got) != 0 {
		t.Fatal("an empty file is nobody logged in")
	}
}

func TestWithConsoleOwner(t *testing.T) {
	if got := withConsoleOwner(nil, "root"); len(got) != 0 {
		t.Fatalf("login window counted as a person: %+v", got)
	}
	got := withConsoleOwner([]tunnel.Logon{{Account: "bo", Line: "ttys001", Remote: true}}, "ana")
	if len(got) != 2 || got[0].Account != "ana" || !got[0].Console {
		t.Fatalf("screen owner not first: %+v", got)
	}
	have := []tunnel.Logon{{Account: "ana", Line: "console", Console: true}}
	if got := withConsoleOwner(have, "ana"); len(got) != 1 {
		t.Fatalf("owner listed twice: %+v", got)
	}
}

func TestParseMacAnswer(t *testing.T) {
	for out, want := range map[string][2]bool{"Allow\n": {true, true}, "No\n": {false, true}, "timeout\n": {false, false}} {
		a, ans, err := parseMacAnswer(out)
		if err != nil || a != want[0] || ans != want[1] {
			t.Fatalf("%q: %v %v %v", out, a, ans, err)
		}
	}
	if a, _, err := parseMacAnswer("execution error"); a || err == nil {
		t.Fatal("an unreadable answer allowed the session")
	}
}
