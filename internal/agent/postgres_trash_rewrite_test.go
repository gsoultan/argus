package agent

import (
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

// What runs in place of a DELETE, DROP or TRUNCATE, and what runs as typed.
func TestTrashRewrite(t *testing.T) {
	cases := []struct{ sql, want string }{
		{"DELETE FROM public.orders o WHERE o.id = $1;",
			"WITH argus_deleted AS (\nDELETE FROM public.orders o WHERE o.id = $1\nRETURNING \"o\".*\n)\n" +
				"SELECT id, kind, source, rows, expires FROM argus_trash_fn.keep_rows(E'\"public\".\"orders\"'::regclass, " +
				"(SELECT jsonb_agg(to_jsonb(d)) FROM argus_deleted d))"},
		{"DROP TABLE IF EXISTS a, \"B\" CASCADE",
			"SELECT id, kind, source, rows, expires FROM argus_trash_fn.drop_tables(ARRAY[E'\"a\"', E'\"B\"']::text[], " +
				"ARRAY[to_regclass(E'\"a\"'), to_regclass(E'\"B\"')]::regclass[], true, true)"},
		{"TRUNCATE x.y", "SELECT id, kind, source, rows, expires FROM argus_trash_fn.empty_tables(ARRAY[E'\"x\".\"y\"']::text[], " +
			"ARRAY[to_regclass(E'\"x\".\"y\"')]::regclass[], false)"},
		{"DROP SCHEMA s", "SELECT id, kind, source, rows, expires FROM argus_trash_fn.drop_schemas(ARRAY[E's']::text[], false, false)"},
		// As typed: rows it returns itself, a view, an INSERT.
		{"DELETE FROM orders WHERE id = 1 RETURNING *", ""},
		{"DROP VIEW v", ""},
		{"INSERT INTO t VALUES (1)", ""},
	}
	for _, tc := range cases {
		if got := trashRewrite(tunnel.ClassifySQL(tc.sql), tc.sql); got != tc.want {
			t.Errorf("%q\n got  %s\n want %s", tc.sql, got, tc.want)
		}
	}
}
