package control

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

// replicaAgents plays both agents of a replica: the primary answers its
// operations, the replica server runs a clone that stops at the verifier.
type replicaAgents struct {
	t   *testing.T
	pub ed25519.PublicKey
	// What the primary says on a check, and whether the clone fails after
	// go.
	check     tunnel.PGReplicaCheck
	cloneFail string

	mu       sync.Mutex
	ops      []string // "<host> <job or op>"
	verifier string
}

func (f *replicaAgents) log(s string) {
	f.mu.Lock()
	f.ops = append(f.ops, s)
	f.mu.Unlock()
}

func (f *replicaAgents) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops...)
}

func (f *replicaAgents) open(_ context.Context, host string, hd tunnel.Header) (tunnel.Stream, tunnel.Answer, error) {
	g, err := (&tunnel.GrantVerifier{Host: host}).Verify(f.pub, hd.Grant)
	if err != nil || hd.Service != tunnel.BackupService || g.Backup == nil {
		f.t.Errorf("a grant the agent would refuse: %v %+v", err, hd)
		return nil, tunnel.Answer{}, err
	}
	if err := g.Backup.Validate(); err != nil {
		f.t.Errorf("a job the agent would refuse: %v", err)
		return nil, tunnel.Answer{}, err
	}
	ctrl, ag := net.Pipe()
	go f.serve(ag, host, *g.Backup)
	return ctrl, tunnel.Answer{OK: true}, nil
}

func (f *replicaAgents) serve(c net.Conn, host string, b tunnel.GrantBackup) {
	defer c.Close()
	result := func(res tunnel.BackupResult) {
		_ = tunnel.WriteFrame(c, tunnel.FrameBackupResult, tunnel.EncodeBackupMessage(res))
	}
	progress := func(stage, detail string) {
		_ = tunnel.WriteFrame(c, tunnel.FrameBackupProgress, tunnel.EncodeBackupMessage(tunnel.BackupProgress{Stage: stage, Detail: detail}))
	}
	r := b.Replica
	if b.Job == tunnel.BackupJobReplicaPrimary {
		f.log(host + " " + r.Op)
		switch r.Op {
		case tunnel.ReplicaCheck:
			data, _ := json.Marshal(f.check)
			result(tunnel.BackupResult{OK: true, Replica: data})
		case tunnel.ReplicaPrepare:
			f.mu.Lock()
			ok := r.Verifier == f.verifier
			f.mu.Unlock()
			if !ok {
				result(tunnel.BackupResult{Error: "not the verifier the replica sent"})
				return
			}
			result(tunnel.BackupResult{OK: true})
		default:
			result(tunnel.BackupResult{OK: true})
		}
		return
	}
	f.log(fmt.Sprintf("%s clone from %s:%d v%d", host, r.PrimaryHost, r.PrimaryPort, r.PrimaryVersion))
	progress("reading", "this server's Postgres")
	v := "SCRAM-SHA-256$4096:c2FsdA==$c3RvcmVk:c2VydmVy"
	f.mu.Lock()
	f.verifier = v
	f.mu.Unlock()
	progress(tunnel.ReplicaStageVerifier, v)
	typ, p, err := tunnel.NewFrameReader(c).Next()
	var req tunnel.BackupRequest
	if err != nil || typ != tunnel.FrameBackupRequest || json.Unmarshal(p, &req) != nil {
		return
	}
	f.log(host + " " + req.Op)
	if req.Op != "go" {
		result(tunnel.BackupResult{Stage: tunnel.ReplicaStageVerifier, Error: "cancelled before the primary was ready"})
		return
	}
	progress("copying", "512/1024 kB (50%)")
	if f.cloneFail != "" {
		result(tunnel.BackupResult{Stage: "copying", Error: f.cloneFail})
		return
	}
	data, _ := json.Marshal(tunnel.PGReplicaCloned{DataDir: "/var/lib/postgresql/16/main",
		Kept: "/var/lib/postgresql/16/main.argus-before-replica", Version: 160004, InRecovery: true})
	result(tunnel.BackupResult{OK: true, Replica: data})
}

// A replica of a managed Postgres: asked by someone with DBA access,
// approved by another person, then run across both agents in order, with
// the password's verifier the only secret that crosses, and the replica
// added to Argus with the primary's access capped at read. A primary that
// needs a restart stops it before anything changes; a copy that fails takes
// the role and slot off the primary again.
func TestDatabaseReplicaOnManagedServers(t *testing.T) {
	a, s, h, admin := enrollAPI(t)
	ctx := context.Background()
	pub, key, _ := ed25519.GenerateKey(nil)
	a.Tunnels = &TunnelHub{GrantKey: key}
	f := &replicaAgents{t: t, pub: pub}
	a.openAgent = f.open
	approver, _ := newAccount(t, s, "approver")
	a.UserTokens["approver-token"] = approver

	primaryAsset, _ := enrolledAsset(t, s, h)
	replicaAsset, _ := enrolledAsset(t, s, h)
	_, _ = s.pool.Exec(ctx, `UPDATE assets SET address = '10.0.1.20' WHERE id::text = $1`, replicaAsset.ID)
	third, _ := enrolledAsset(t, s, h)
	live := func() {
		if _, err := s.pool.Exec(ctx, `UPDATE agent_credentials SET tunnel_seen_at = now()
			WHERE lower(hostname) IN (lower($1), lower($2), lower($3))`, primaryAsset.Hostname, replicaAsset.Hostname, third.Hostname); err != nil {
			t.Fatal(err)
		}
	}
	live()
	primary, err := s.CreateDatabase(ctx, Database{AssetID: primaryAsset.ID, Engine: "postgres", Host: "/var/run/postgresql",
		Port: 5432, Name: "orders"}, admin)
	if err != nil {
		t.Fatal(err)
	}
	dba := "dba-" + primary.ID[:8] + "@example.com"
	if err := s.SetDBAssignment(ctx, DBAssignment{DatabaseID: primary.ID, UserEmail: &dba, Level: tunnel.LevelOwner}, admin); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/databases/" + primary.ID + "/replicas"
	ask := func(body string, want int) DatabaseReplica {
		t.Helper()
		w := do(t, h, http.MethodPost, path, "admin-token", body)
		if w.Code != want {
			t.Fatalf("ask: %d %s, want %d", w.Code, w.Body, want)
		}
		var r DatabaseReplica
		_ = json.Unmarshal(w.Body.Bytes(), &r)
		return r
	}
	on := func(asset Asset, name, extra string) string {
		return `{"assetId":"` + asset.ID + `","name":"` + name + `","reason":"reads are busy"` + extra + `}`
	}
	decide := func(rep DatabaseReplica, decision string) {
		t.Helper()
		w := do(t, h, http.MethodPost, "/api/v1/requests/"+*rep.RequestID+"/decision", "approver-token", `{"decision":"`+decision+`","note":"not this week"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("decide: %d %s", w.Code, w.Body)
		}
	}
	wait := func(id string) DatabaseReplica {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			r, err := s.Replica(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if r.State == "done" || r.State == "failed" || time.Now().After(deadline) {
				return r
			}
		}
	}

	// Refused before anything is asked.
	ask(on(primaryAsset, "r1", ""), http.StatusBadRequest)
	ask(on(replicaAsset, "Bad-Name", ""), http.StatusBadRequest)
	ask(`{"assetId":"`+replicaAsset.ID+`","name":"r1"}`, http.StatusBadRequest)

	// Asked, waiting for a second person; nothing has run.
	f.check = tunnel.PGReplicaCheck{VersionNum: 160004, WalLevel: "replica",
		Missing: []string{"pg_hba.conf does not let 10.0.1.20 replicate as argus_repl_r1; add this line ..."}}
	_, _ = s.pool.Exec(ctx, `UPDATE assets SET address = '10.0.1.10' WHERE id::text = $1`, primaryAsset.ID)
	rep := ask(on(replicaAsset, "r1", ""), http.StatusAccepted)
	if rep.State != "pending" || rep.RequestID == nil || rep.ReplicaAddress != "10.0.1.20" || rep.PrimaryAddress != "10.0.1.10" {
		t.Fatalf("asked: %+v", rep)
	}
	ask(on(replicaAsset, "r2", ""), http.StatusConflict) // that server is taken
	if len(f.seen()) != 0 {
		t.Fatalf("ran before approval: %v", f.seen())
	}

	// Approved: check, clone to the verifier, prepare with it, allow (no
	// line let it in), go, done.
	decide(rep, "approved")
	done := wait(rep.ID)
	if done.State != "done" || done.ReplicaDatabaseID == nil {
		t.Fatalf("done: %+v", done)
	}
	want := []string{primaryAsset.Hostname + " check",
		replicaAsset.Hostname + " clone from 10.0.1.10:5432 v16",
		primaryAsset.Hostname + " prepare", primaryAsset.Hostname + " allow",
		replicaAsset.Hostname + " go"}
	if got := f.seen(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("order:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	added, err := s.Database(ctx, *done.ReplicaDatabaseID)
	if err != nil || added.AssetID != replicaAsset.ID || added.Name != "orders" {
		t.Fatalf("added: %+v %v", added, err)
	}
	got, _ := s.DBAssignments(ctx, added.ID)
	if len(got) != 1 || got[0].Level != tunnel.LevelRead || deref(got[0].UserEmail) != dba {
		t.Fatalf("access: %+v", got)
	}

	// A primary that needs a restart stops it at the check.
	f.mu.Lock()
	f.ops = nil
	f.mu.Unlock()
	f.check.Missing = []string{"wal_level is minimal; set it to replica in postgresql.conf and restart"}
	_, _ = s.pool.Exec(ctx, `UPDATE assets SET address = '10.0.1.30' WHERE id::text = $1`, third.ID)
	live()
	r2 := ask(on(third, "r2", ""), http.StatusAccepted)
	decide(r2, "approved")
	if r := wait(r2.ID); r.State != "failed" || r.Stage != "checking" || !strings.Contains(r.Error, "never restarts a primary") ||
		!strings.Contains(r.Error, "wal_level") {
		t.Fatalf("restart needed: %+v", r)
	}
	if got := f.seen(); len(got) != 1 {
		t.Fatalf("went on past the check: %v", got)
	}

	// A copy that fails takes the role and slot off the primary again.
	f.mu.Lock()
	f.ops = nil
	f.mu.Unlock()
	f.check.Missing, f.check.HBAOK = nil, true
	f.cloneFail = "pg_basebackup: could not connect"
	_, _ = s.pool.Exec(ctx, `DELETE FROM database_replicas WHERE id = $1::uuid`, r2.ID)
	r3 := ask(on(third, "r3", ""), http.StatusAccepted)
	decide(r3, "approved")
	if r := wait(r3.ID); r.State != "failed" || !strings.Contains(r.Error, "could not connect") {
		t.Fatalf("copy failed: %+v", r)
	}
	if got := strings.Join(f.seen(), "\n"); !strings.Contains(got, primaryAsset.Hostname+" drop") || strings.Contains(got, " allow") {
		t.Fatalf("after a failed copy:\n%s", got)
	}

	// Denied: nothing runs.
	f.mu.Lock()
	f.ops = nil
	f.mu.Unlock()
	r4 := ask(on(third, "r4", ""), http.StatusAccepted)
	decide(r4, "denied")
	if r, _ := s.Replica(ctx, r4.ID); r.State != "denied" || !strings.Contains(r.Error, "not this week") || len(f.seen()) != 0 {
		t.Fatalf("denied: %+v %v", r, f.seen())
	}
	list := do(t, h, http.MethodGet, path, "admin-token", "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"name":"r1"`) {
		t.Fatalf("list: %d %s", list.Code, list.Body)
	}
}
