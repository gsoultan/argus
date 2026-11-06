//go:build !windows

package agent

import (
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

// An owner or dba session can change what an application's own role owns:
// tables made by the app before anyone used Argus. A role that would make the
// level more than an ordinary one -- a superuser, or a role that can create
// roles -- is never handed out.
func TestPostgresOwnerAltersObjectsOfExistingRoles(t *testing.T) {
	dsn, _, _, dbname := pgTest(t)
	superExec(t, dsn,
		"DROP TABLE IF EXISTS argus_t_quot, argus_t_super, argus_t_owns_cr",
		"DROP ROLE IF EXISTS ana_sari",
		"DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'argus_t_owns') THEN CREATE ROLE argus_t_owns LOGIN; END IF; END $$",
		"DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'argus_t_owns_su') THEN CREATE ROLE argus_t_owns_su SUPERUSER; END IF; END $$",
		"DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'argus_t_owns_cr') THEN CREATE ROLE argus_t_owns_cr CREATEROLE; END IF; END $$",
		"ALTER ROLE argus_t_owns NOSUPERUSER",
		"GRANT CREATE ON SCHEMA public TO argus_t_owns, argus_t_owns_cr",
		"CREATE TABLE argus_t_quot (id int)",
		"ALTER TABLE argus_t_quot OWNER TO argus_t_owns",
		"CREATE TABLE argus_t_super (id int)",
		"ALTER TABLE argus_t_super OWNER TO argus_t_owns_su",
		"CREATE TABLE argus_t_owns_cr (id int)",
		"ALTER TABLE argus_t_owns_cr OWNER TO argus_t_owns_cr")
	t.Cleanup(func() {
		superExec(t, dsn, "DROP TABLE IF EXISTS argus_t_quot, argus_t_super, argus_t_owns_cr",
			"DROP OWNED BY argus_t_owns, argus_t_owns_su, argus_t_owns_cr",
			"DROP ROLE argus_t_owns, argus_t_owns_su, argus_t_owns_cr")
	})

	pg16 := superQuery(t, dsn, "SELECT current_setting('server_version_num')::int >= 160000")[0][0] == "t"
	for _, level := range []string{tunnel.LevelOwner, tunnel.LevelDBA} {
		t.Run(level, func(t *testing.T) {
			h := openPG(t, level, nil)
			defer h.close()
			for i, s := range []string{
				"ALTER TABLE argus_t_quot ADD COLUMN IF NOT EXISTS total numeric",
				"CREATE INDEX IF NOT EXISTS argus_t_quot_total ON argus_t_quot (total)",
				"ALTER TABLE argus_t_quot DROP COLUMN total",
			} {
				if _, _, final := h.run(uint32(1+i), s, 0); final.Type != "done" {
					t.Fatalf("%s: %+v", s, final)
				}
			}
			// The level is not a way to become someone more powerful.
			refused := []string{"ALTER TABLE argus_t_super ADD COLUMN x int", "ALTER TABLE argus_t_owns_cr ADD COLUMN x int"}
			if pg16 {
				// Before 16 a member may always SET ROLE; there the role
				// was checked to carry nothing more than its objects.
				refused = append(refused, "SET ROLE argus_t_owns")
			}
			for i, s := range refused {
				if _, _, final := h.run(uint32(10+i), s, 0); final.Type != "error" || final.Code != "42501" {
					t.Fatalf("%s must be refused: %+v", s, final)
				}
			}
		})
	}
	// Nothing an Argus level holds can be used to become a superuser or to
	// make roles, however far the memberships reach.
	got := superQuery(t, dsn, `SELECT r.rolname FROM pg_roles l, pg_roles r
		WHERE l.rolname LIKE 'argus\_owner\_%' AND pg_has_role(l.oid, r.oid, 'MEMBER')
		  AND (r.rolsuper OR r.rolcreaterole)`)
	if len(got) != 0 {
		t.Fatalf("the owner level reaches a powerful role: %v", got)
	}
	if !strings.HasPrefix(levelRole(tunnel.LevelOwner, "app"), "argus_owner_") {
		t.Fatal("level role naming changed")
	}

	// A role that became a superuser since is taken away at the next open.
	superExec(t, dsn, "ALTER ROLE argus_t_owns SUPERUSER")
	h := openPG(t, tunnel.LevelOwner, nil)
	if _, _, final := h.run(1, "ALTER TABLE argus_t_quot ADD COLUMN x int", 0); final.Type != "error" || final.Code != "42501" {
		t.Fatalf("a superuser's table must stay out of reach: %+v", final)
	}
	h.close()
	got = superQuery(t, dsn, `SELECT count(*) FROM pg_auth_members m JOIN pg_roles g ON g.oid = m.roleid
		JOIN pg_roles l ON l.oid = m.member WHERE g.rolname = 'argus_t_owns' AND l.rolname LIKE 'argus\_owner\_%'`)
	if got[0][0] != "0" {
		t.Fatalf("the membership was kept: %v", got)
	}

	// Back to ordinary, it is given again, and a DROP of it goes to the trash.
	superExec(t, dsn, "ALTER ROLE argus_t_owns NOSUPERUSER")
	h = openPG(t, tunnel.LevelOwner, nil)
	defer h.close()
	if _, _, final := h.do(tunnel.PGRequest{ID: 1, Op: "run", SQL: "DROP TABLE argus_t_quot", Confirm: dbname + "/argus_t_quot"}); final.Type != "done" || len(final.Trash) != 1 {
		t.Fatalf("drop: %+v", final)
	}
}
