package agent

import "testing"

// A literal must mean the same with standard_conforming_strings on or off.
// With it off, a backslash before a quote used to end the string early.
func TestQuoteLiteralSurvivesBackslashes(t *testing.T) {
	for in, want := range map[string]string{
		`plain`:            `E'plain'`,
		`it's`:             `E'it''s'`,
		`a\'; DROP ROLE x`: `E'a\\''; DROP ROLE x'`,
		`back\slash`:       `E'back\\slash'`,
	} {
		if got := quoteLiteral(in); got != want {
			t.Errorf("quoteLiteral(%q) = %s, want %s", in, got, want)
		}
	}
}
