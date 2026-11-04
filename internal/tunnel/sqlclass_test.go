package tunnel

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// The cases live in a file the console's tests read too, so the two
// classifiers cannot drift apart without one of them failing.
type sqlClassCase struct {
	SQL  string   `json:"sql"`
	Want SQLClass `json:"want"`
}

func loadSQLClassCases(t *testing.T) []sqlClassCase {
	t.Helper()
	b, err := os.ReadFile("testdata/sqlclass.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []sqlClassCase
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 20 {
		t.Fatalf("only %d cases", len(cases))
	}
	return cases
}

func TestClassifySQL(t *testing.T) {
	for _, tc := range loadSQLClassCases(t) {
		got := ClassifySQL(tc.SQL)
		if !reflect.DeepEqual(got, tc.Want) {
			t.Errorf("%q\n got  %+v\n want %+v", tc.SQL, got, tc.Want)
		}
	}
}

func TestGuardSQL(t *testing.T) {
	cases := []struct {
		sql, confirm string
		code         string
	}{
		{"DELETE FROM orders", "", GuardNoWhere},
		{"DELETE FROM orders", "orders/orders", ""},
		{"DELETE FROM orders", "orders/order", GuardNoWhere},
		{"delete from orders where id = 1", "", ""},
		{"UPDATE orders SET status = 'x'", "", GuardNoWhere},
		{"UPDATE orders SET status = (SELECT s FROM t WHERE t.id = 1)", "", GuardNoWhere},
		{"UPDATE orders SET status = 'x' WHERE id = 1", "", ""},
		{"TRUNCATE orders", "", GuardDrop},
		{"TRUNCATE orders", "orders/orders", ""},
		{"DROP TABLE orders CASCADE", "", GuardDrop},
		{"DROP TABLE public.orders, refunds", "orders/public.orders, refunds", ""},
		{"DROP SCHEMA billing", "", GuardDrop},
		{"DROP DATABASE orders", "", GuardDropDB},
		{"DROP DATABASE orders", "orders/orders", GuardDropDB},
		{"DROP INDEX orders_status_idx", "", ""},
		{"SELECT * FROM orders", "", ""},
		{"-- DELETE FROM orders\nSELECT 1", "", ""},
		{"SELECT 'DELETE FROM orders'", "", ""},
		{"WITH gone AS (DELETE FROM orders RETURNING id) SELECT count(*) FROM gone", "", ""},
	}
	for _, tc := range cases {
		g := GuardSQL(ClassifySQL(tc.sql), "orders", tc.confirm)
		code := ""
		if g != nil {
			code = g.Code
			if g.Reason == "" {
				t.Errorf("%q: a refusal with no reason", tc.sql)
			}
		}
		if code != tc.code {
			t.Errorf("%q confirm %q: got %q want %q", tc.sql, tc.confirm, code, tc.code)
		}
	}
}

func FuzzClassifySQL(f *testing.F) {
	for _, s := range []string{"DELETE FROM t", "/* /* */ x */ drop table t", "$a$ $b$ $a$", "e'\\''", `"a""b"`, "((("} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		_ = ClassifySQL(s) // never panics, always ends
	})
}
