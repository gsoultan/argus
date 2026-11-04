package tunnel

import "testing"

// A test restore's custom check is one read-only SELECT. Postgres enforces
// that too, in a READ ONLY transaction; this is what tells the admin when
// they save it.
func TestReadOnlyCheckSQL(t *testing.T) {
	ok := []string{
		"SELECT count(*) > 0 FROM orders",
		"select count(*) from orders;",
		"WITH paid AS (SELECT * FROM orders WHERE state = 'paid') SELECT count(*) FROM paid",
		"SELECT ';' = ';'",
		"SELECT 1 -- ; drop table orders",
		`SELECT count(*) FROM "delete"`,
		"SELECT $$;$$ <> ''",
	}
	for _, q := range ok {
		if err := ReadOnlyCheckSQL(q); err != nil {
			t.Errorf("%q refused: %v", q, err)
		}
	}
	bad := []string{
		"",
		"DELETE FROM orders",
		"UPDATE orders SET state = 'x'",
		"SELECT 1; DROP TABLE orders",
		"WITH gone AS (DELETE FROM orders RETURNING *) SELECT count(*) FROM gone",
		"INSERT INTO orders DEFAULT VALUES",
		"VACUUM orders",
	}
	for _, q := range bad {
		if err := ReadOnlyCheckSQL(q); err == nil {
			t.Errorf("%q accepted", q)
		}
	}
}
