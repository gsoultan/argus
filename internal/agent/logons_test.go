package agent

import (
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

func utmpRec(typ int16, user, line, host string, sec int32) []byte {
	r := make([]byte, utmpRecord)
	binary.LittleEndian.PutUint16(r[0:2], uint16(typ))
	binary.LittleEndian.PutUint32(r[4:8], 4242)
	copy(r[8:40], line)
	copy(r[44:76], user)
	copy(r[76:332], host)
	binary.LittleEndian.PutUint32(r[340:344], uint32(sec))
	return r
}

func TestParseUtmp(t *testing.T) {
	var b []byte
	b = append(b, utmpRec(2, "reboot", "~", "", 1)...) // boot record
	b = append(b, utmpRec(utmpUserProcess, "ana", "pts/0", "10.0.0.7", 1_700_000_000)...)
	b = append(b, utmpRec(utmpUserProcess, "bo", "tty1", "", 0)...)
	b = append(b, utmpRec(8, "gone", "pts/1", "10.0.0.8", 1)...) // dead process
	b = append(b, 1, 2, 3)                                       // a torn last record is ignored
	got := parseUtmp(b)
	if len(got) != 2 {
		t.Fatalf("want 2 logons, got %+v", got)
	}
	if got[0].Account != "ana" || got[0].Line != "pts/0" || got[0].From != "10.0.0.7" || !got[0].Remote || got[0].Console {
		t.Errorf("ana: %+v", got[0])
	}
	if got[0].LogonAt == nil || got[0].LogonAt.Unix() != 1_700_000_000 {
		t.Errorf("ana's logon time: %v", got[0].LogonAt)
	}
	if got[0].Session != 0 {
		t.Errorf("a Linux login has no Windows session: %+v", got[0])
	}
	if got[1].Account != "bo" || !got[1].Console || got[1].Remote || got[1].LogonAt != nil {
		t.Errorf("bo: %+v", got[1])
	}
}

type fakeLogons struct {
	res       tunnel.LogonsResult
	signedOut []uint32
}

func (f *fakeLogons) List(context.Context) (tunnel.LogonsResult, error) { return f.res, nil }
func (f *fakeLogons) SignOut(_ context.Context, s uint32) error {
	f.signedOut = append(f.signedOut, s)
	return nil
}

func TestSignOutOnlyAnIdleSessionOfTheSameAccount(t *testing.T) {
	logons := []tunnel.Logon{
		{Session: 1, Account: `HOST\admin`, State: tunnel.LogonActive, Console: true},
		{Session: 2, Account: `HOST\ana`, State: tunnel.LogonDisconnected, Remote: true},
		{Session: 3, Account: `HOST\bo`, State: tunnel.LogonActive, Remote: true},
		{Session: 4, Account: `HOST\cy`, State: tunnel.LogonDisconnected, Console: true},
	}
	cases := []struct {
		name    string
		q       tunnel.GrantLogons
		refused string
	}{
		{"idle", tunnel.GrantLogons{Op: tunnel.LogonsSignOut, Session: 2, Account: `HOST\ana`}, ""},
		{"in use", tunnel.GrantLogons{Op: tunnel.LogonsSignOut, Session: 3, Account: `HOST\bo`}, "using that session"},
		{"console", tunnel.GrantLogons{Op: tunnel.LogonsSignOut, Session: 4, Account: `HOST\cy`}, "own screen"},
		{"changed hands", tunnel.GrantLogons{Op: tunnel.LogonsSignOut, Session: 2, Account: `HOST\bo`}, "now HOST\\ana's"},
		{"gone", tunnel.GrantLogons{Op: tunnel.LogonsSignOut, Session: 9, Account: `HOST\ana`}, "already ended"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeLogons{res: tunnel.LogonsResult{Platform: "windows", Logons: logons, RemoteLimit: 2}}
			res := answerLogons(context.Background(), f, c.q)
			if c.refused == "" {
				if !res.OK || res.SignedOut != c.q.Session || len(f.signedOut) != 1 {
					t.Fatalf("want signed out: %+v %v", res, f.signedOut)
				}
				return
			}
			if res.OK || !strings.Contains(res.Error, c.refused) || len(f.signedOut) != 0 {
				t.Fatalf("want refused with %q: %+v, signed out %v", c.refused, res, f.signedOut)
			}
		})
	}
}

func TestListLogons(t *testing.T) {
	f := &fakeLogons{res: tunnel.LogonsResult{Platform: "windows", RemoteLimit: 2, Edition: "server",
		Logons: []tunnel.Logon{{Session: 2, Account: "a", State: tunnel.LogonActive}}}}
	res := answerLogons(context.Background(), f, tunnel.GrantLogons{Op: tunnel.LogonsList})
	if !res.OK || len(res.Logons) != 1 || res.RemoteLimit != 2 || res.Edition != "server" {
		t.Fatalf("%+v", res)
	}
}

type failingLogons struct{}

func (failingLogons) List(context.Context) (tunnel.LogonsResult, error) {
	return tunnel.LogonsResult{}, errors.New("boom")
}
func (failingLogons) SignOut(context.Context, uint32) error { return nil }

func TestListLogonsFailure(t *testing.T) {
	res := answerLogons(context.Background(), failingLogons{}, tunnel.GrantLogons{Op: tunnel.LogonsList})
	if res.OK || res.Error != "boom" {
		t.Fatalf("%+v", res)
	}
}

func TestGrantLogonsValidate(t *testing.T) {
	bad := []tunnel.GrantLogons{
		{Op: "reboot"},
		{Op: tunnel.LogonsList, Session: 2},
		{Op: tunnel.LogonsSignOut, Account: "a"},
		{Op: tunnel.LogonsSignOut, Session: 2},
		{Op: tunnel.LogonsSignOut, Session: 2, Account: strings.Repeat("a", 257)},
	}
	for _, q := range bad {
		if q.Validate() == nil {
			t.Errorf("accepted %+v", q)
		}
	}
	if (tunnel.GrantLogons{Op: tunnel.LogonsList}).Validate() != nil {
		t.Error("refused a plain list")
	}
}
