package agent

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// peerCollector is a collector whose socket says the connection came from
// uid as process pid, with an account list of its own.
func peerCollector(t *testing.T, uid, pid int) (*Collector, string) {
	t.Helper()
	passwd := filepath.Join(t.TempDir(), "passwd")
	if err := os.WriteFile(passwd, []byte(strings.Join([]string{
		"root:x:0:0:root:/root:/bin/bash",
		"ops:x:1000:1000:ops:/home/ops:/bin/bash",
		"ana:x:1001:1001:Argus personal account:/home/ana:/bin/bash",
		"toor:x:1000:1000:alias of ops:/home/ops:/bin/bash",
	}, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return testCollector(t, func(c *Collector) {
		c.PasswdFile = passwd
		c.PeerOf = func(net.Conn) (ShimPeer, error) {
			return ShimPeer{UID: uid, PID: pid, Known: true}, nil
		}
	})
}

func claim(t *testing.T, sock, principal string, pid int) Reply {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := json.NewEncoder(conn).Encode(SessionStart{
		Type: "start", Principal: principal, Hostname: "db-01", Origin: Direct,
		ClientAddr: "10.0.0.9", StartedAt: time.Now().UTC(), Cols: 80, Rows: 24, PID: pid,
	}); err != nil {
		t.Fatal(err)
	}
	return readReply(t, conn, json.NewDecoder(bufio.NewReader(conn)))
}

// Root cause: the collector took the account and pid from the start
// message, which the recorded user writes. A local user (uid 1000, ops)
// could open a recording as root or as another person's personal account.
func TestALocalUserCannotOpenARecordingAsAnotherAccount(t *testing.T) {
	c, sock := peerCollector(t, 1000, 4242)
	defer c.CloseWithin(time.Second)

	for _, who := range []string{"root", "ana", "someone-in-ldap"} {
		r := claim(t, sock, who, 4242)
		if r.Record || r.SessionID != "" || !strings.Contains(r.Error, "uid 1000 connected") {
			t.Errorf("uid 1000 claiming %s got %+v, want refused", who, r)
		}
	}
	if n := recordings(t, c); n != 0 {
		t.Errorf("%d recordings were opened for forged logins", n)
	}
}

func TestALocalUserCannotPointTheTraceAtAnotherProcess(t *testing.T) {
	c, sock := peerCollector(t, 1000, 4242)
	defer c.CloseWithin(time.Second)

	r := claim(t, sock, "ops", 1)
	if r.Record || !strings.Contains(r.Error, "process 4242 connected") {
		t.Errorf("a claim of pid 1 from pid 4242 got %+v, want refused", r)
	}
}

func TestALoginAsItsOwnAccountIsRecorded(t *testing.T) {
	c, sock := peerCollector(t, 1000, 4242)
	defer c.CloseWithin(time.Second)

	// Its own name, an alias with the same uid, and the shim's fallback.
	for _, who := range []string{"ops", "toor", "uid:1000"} {
		if r := claim(t, sock, who, 4242); !r.Record || r.SessionID == "" {
			t.Errorf("uid 1000 as %s got %+v, want recorded", who, r)
		}
	}
}

// A directory user is not in the local passwd; with a uid no local account
// has, the agent cannot contradict it and records the login as before.
func TestADirectoryUserIsRecorded(t *testing.T) {
	c, sock := peerCollector(t, 70001, 4242)
	defer c.CloseWithin(time.Second)

	if r := claim(t, sock, "budi", 4242); !r.Record || r.SessionID == "" {
		t.Errorf("a directory user got %+v, want recorded", r)
	}
	if r := claim(t, sock, "root", 4242); r.Record {
		t.Errorf("a directory user claiming root got %+v, want refused", r)
	}
}

// The real kernel answer, on a real socket: the test process itself.
func TestTheSocketPeerIsThisProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no peer credentials; the socket is closed to other accounts instead")
	}
	dir, err := os.MkdirTemp(shortTempRoot, "argus")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ln, err := net.Listen("unix", filepath.Join(dir, "p.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cc, err := net.Dial("unix", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	srv, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	p, err := socketPeer(srv)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Known || p.UID != os.Geteuid() || p.PID != os.Getpid() {
		t.Errorf("peer = %+v, want uid %d pid %d", p, os.Geteuid(), os.Getpid())
	}
}
