package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gsoultan/argus/internal/tunnel"
)

// Opening a session behind a transaction that holds a table's lock gives up
// and says what is in the way, rather than waiting for good while the console
// times out with "i/o deadline reached".
func TestOpeningBehindAHeldLockSaysSo(t *testing.T) {
	dsn, _, _, dbname := pgTest(t)
	ctx := context.Background()
	holder, err := pgconn.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close(ctx)
	if _, err := holder.Exec(ctx, `DROP TABLE IF EXISTS lockwait_held;
		CREATE TABLE lockwait_held (id int)`).ReadAll(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = holder.Exec(context.Background(), "ROLLBACK; DROP TABLE IF EXISTS lockwait_held").ReadAll()
	})
	if _, err := holder.Exec(ctx, "BEGIN; LOCK TABLE lockwait_held IN ACCESS EXCLUSIVE MODE").ReadAll(); err != nil {
		t.Fatal(err)
	}

	verifier, err := scramVerifier("not-a-real-password-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	octx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	start := time.Now()
	res := DSNAdmin(func(string) string { return dsn })(octx, PGAdminRequest{Op: "open",
		Database: dbname, Role: "lock_wait", Owner: "ana.sari@corp.example", Level: tunnel.LevelDBA,
		Verifier: verifier, ValidUntil: time.Now().Add(time.Hour)})
	took := time.Since(start)
	if res.OK {
		t.Fatal("the open went through a table another session holds locked")
	}
	if !strings.Contains(res.Error, "a lock another session holds") || !strings.Contains(res.Error, dbname) {
		t.Errorf("error = %q", res.Error)
	}
	if took > 30*time.Second {
		t.Errorf("gave up after %s; the agent's own bound is 30s", took)
	}
}
