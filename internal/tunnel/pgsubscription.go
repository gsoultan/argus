package tunnel

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// pgLSN is a WAL location as Postgres prints it.
var pgLSN = regexp.MustCompile(`^[0-9A-Fa-f]{1,8}/[0-9A-Fa-f]{1,8}$`)

// ValidLSN says s is a WAL location such as 0/1D5B8F0.
func ValidLSN(s string) bool { return pgLSN.MatchString(s) }

// PGSubscriptionSkipSQL is what subscription.skip runs: skip the one
// publisher transaction that finished at lsn, then turn the subscription back
// on. The agent runs it and the control plane asks for approval of the same
// text, so both build it here.
func PGSubscriptionSkipSQL(name, lsn string) ([]string, error) {
	n, err := QuoteIdent(name)
	if err != nil {
		return nil, err
	}
	if !ValidLSN(lsn) {
		return nil, fmt.Errorf("%q is not a WAL location like 0/1D5B8F0", lsn)
	}
	return []string{
		"ALTER SUBSCRIPTION " + n + " SKIP (lsn = '" + strings.ToUpper(lsn) + "')",
		"ALTER SUBSCRIPTION " + n + " ENABLE",
	}, nil
}

// PGSubscriptionSkipText is the text an approval of subscription.skip is
// asked for, and what the agent records.
func PGSubscriptionSkipText(op *PGAdminOp) (string, error) {
	if op == nil || op.Kind != AdminSubscriptionSkip {
		return "", errors.New("not a skip")
	}
	stmts, err := PGSubscriptionSkipSQL(op.Name, op.LSN)
	if err != nil {
		return "", err
	}
	return strings.Join(stmts, ";\n"), nil
}
