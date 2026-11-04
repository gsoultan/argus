package tunnel

import "testing"

func TestNeedsApproval(t *testing.T) {
	for sql, want := range map[string]bool{
		"SELECT * FROM orders": false,
		"select count(*) from orders where note = 'insert into x'": false,
		"WITH a AS (SELECT 1) SELECT * FROM a":                     false,
		"WITH d AS (DELETE FROM t RETURNING *) SELECT * FROM d":    true,
		"BEGIN":                    false,
		"COMMIT":                   false,
		"SHOW search_path":         false,
		"EXPLAIN SELECT 1":         false,
		"EXPLAIN ANALYZE SELECT 1": false,
		"EXPLAIN (ANALYZE) DELETE FROM t WHERE id = 1":                      true,
		"EXPLAIN DELETE FROM t WHERE id = 1":                                false,
		"INSERT INTO t VALUES (1)":                                          true,
		"UPDATE t SET a = 1 WHERE id = 2":                                   true,
		"DELETE FROM t WHERE id = 2":                                        true,
		"TRUNCATE t":                                                        true,
		"DROP TABLE t":                                                      true,
		"CREATE INDEX ON t (a)":                                             true,
		"ALTER TABLE t ADD COLUMN b int":                                    true,
		"GRANT SELECT ON t TO bob":                                          true,
		"COPY t FROM STDIN":                                                 true,
		"SELECT * INTO copy_of_t FROM t":                                    true,
		"SELECT * FROM argus_trash_fn.restore($1::bigint)":                  true,
		"SELECT argus_trash_fn.drop_tables(ARRAY['t'], NULL, false, false)": true,
		"SELECT id, kind FROM argus_trash_fn.list()":                        false,
		"": false,
	} {
		if got := NeedsApproval(ClassifySQL(sql), sql); got != want {
			t.Errorf("%q: %v, want %v", sql, got, want)
		}
	}
}

func TestStatementDigest(t *testing.T) {
	one := "1"
	if StatementDigest("SELECT 1", nil) != StatementDigest("SELECT 1", []*string{}) {
		t.Fatal("nil and empty values differ")
	}
	if StatementDigest("DELETE FROM t WHERE id = $1", []*string{&one}) == StatementDigest("DELETE FROM t WHERE id = $1", []*string{nil}) {
		t.Fatal("a value and NULL are the same statement")
	}
	if StatementDigest("SELECT 1", nil) == StatementDigest("SELECT 1 ", nil) {
		t.Fatal("different text, same digest")
	}
}
