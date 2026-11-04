//go:build !windows

package agent

import (
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

// The results grid deletes a row by its key, with a parameter, inside the
// transaction a save opens, and with no RETURNING so the trash keeps it.
func TestPostgresGridDeleteGoesToTheTrash(t *testing.T) {
	dsn, _, _, _ := pgTest(t)
	superExec(t, dsn, "DROP TABLE IF EXISTS argus_t_gd CASCADE")
	h := openPG(t, tunnel.LevelOwner, nil)
	s := func(v string) *string { return &v }
	for i, q := range []string{"CREATE TABLE argus_t_gd (id int PRIMARY KEY, v text)", "INSERT INTO argus_t_gd VALUES (1, 'a'), (2, 'b')", "BEGIN"} {
		if _, _, a := h.run(uint32(100+i), q, 0); a.Type != "done" {
			t.Fatalf("%s: %+v", q, a)
		}
	}
	_, _, final := h.do(tunnel.PGRequest{ID: 1, Op: "run", SQL: "DELETE FROM public.argus_t_gd\nWHERE id = CAST($1 AS integer)", Params: []*string{s("2")}})
	if final.Type != "done" || final.Tag != "DELETE 1" || len(final.Trash) != 1 || final.Trash[0].Rows != 1 {
		t.Fatalf("delete: %+v", final)
	}
	if _, _, a := h.run(2, "COMMIT", 0); a.Type != "done" {
		t.Fatalf("commit: %+v", a)
	}
	if n := superQuery(t, dsn, "SELECT count(*) FROM argus_t_gd")[0][0]; n != "1" {
		t.Fatalf("%s rows left", n)
	}
	// A row that is gone deletes nothing, which the save reports.
	_, _, final = h.do(tunnel.PGRequest{ID: 3, Op: "run", SQL: "DELETE FROM public.argus_t_gd\nWHERE id = CAST($1 AS integer)", Params: []*string{s("2")}})
	if final.Type != "done" || final.Tag != "DELETE 0" {
		t.Fatalf("delete again: %+v", final)
	}
}
