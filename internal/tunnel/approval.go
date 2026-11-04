package tunnel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

/*
Statements that need someone else's approval on a production database.

On a database labelled production, anything that changes it runs only under
an approved request for exactly that statement, whatever the person's level:
Owner and DBA too. Reading does not. The rule is by what a statement is, not
by who sends it, so the control plane (which enforces it, in the relay) and
the console (which asks before it sends) apply the same one; the console's
copy is web/src/lib/approval.ts.

A function a SELECT calls can still write; the level's grants are what stop
that, as they always have. This rule is the second pair of eyes on what the
person can see they are doing.
*/

// GuardNeedsApproval is the answer's code when a statement on production
// arrives without a usable approval.
const GuardNeedsApproval = "AR007"

// readVerbs are the statements that change nothing in the database.
var readVerbs = map[string]bool{
	"": true, "select": true, "with": true, "show": true, "explain": true, "values": true, "table": true,
	"begin": true, "start": true, "commit": true, "end": true, "rollback": true, "abort": true,
	"savepoint": true, "release": true, "set": true, "reset": true, "discard": true,
	"fetch": true, "move": true, "close": true, "declare": true, "listen": true, "unlisten": true,
	"prepare": true, "deallocate": true,
}

// trashReads are the trash's functions that only read.
var trashReads = map[string]bool{"list": true}

// NeedsApproval says whether sql, classified as c, changes the database.
func NeedsApproval(c SQLClass, sql string) bool {
	if c.Writes || !readVerbs[c.Verb] {
		return true
	}
	toks := lexSQL(sql)
	for i, t := range toks {
		if t.punct != 0 {
			continue
		}
		switch {
		// Putting something back from the trash, or calling its functions
		// by hand, is a change like the statement that put it there.
		case t.text == "argus_trash_fn" && i+2 < len(toks) && toks[i+1].punct == '.' && !trashReads[toks[i+2].text]:
			return true
		// SELECT ... INTO makes a table.
		case c.Verb == "select" && t.text == "into" && !t.quoted && t.depth == 0:
			return true
		}
	}
	// EXPLAIN ANALYZE runs what it explains.
	if c.Verb == "explain" {
		analyze, writes := false, false
		for _, t := range toks {
			if t.punct != 0 || t.quoted {
				continue
			}
			switch t.text {
			case "analyze", "analyse":
				analyze = true
			case "insert", "update", "delete", "merge", "truncate", "create", "drop", "alter":
				writes = true
			}
		}
		return analyze && writes
	}
	return false
}

// StatementDigest is what an approval for one statement is checked by: its
// text and its values, exactly as sent. A nil list and an empty one are the
// same statement.
func StatementDigest(sql string, params []*string) string {
	if params == nil {
		params = []*string{}
	}
	p, _ := json.Marshal(params)
	h := sha256.New()
	h.Write([]byte(sql))
	h.Write([]byte{'\n'})
	h.Write(p)
	return hex.EncodeToString(h.Sum(nil))
}
