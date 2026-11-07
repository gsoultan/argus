package tunnel

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

/*
What a database statement is, near enough to stop an accident.

This is not access control. What a person may do is what their level role's
grants let Postgres do, and nothing here widens or narrows that. The guard
exists for the person who has the right to empty a table and does it by
mistake: an UPDATE or DELETE without a WHERE, a DROP of the wrong table, a
TRUNCATE typed in the wrong tab. It reads the statement's shape, never its
meaning, and when it cannot tell it says nothing and lets Postgres decide.

ClassifySQL is shared: the agent asks it before running a statement, the
control plane may ask it of a check query, and the console mirrors the same
cases in its tests.
*/

// SQLClass is what a statement looks like from its words at the top level.
type SQLClass struct {
	// Verb is the main statement's first word, lower case: select, insert,
	// update, delete, merge, truncate, drop, create, alter, begin, ... Empty
	// when the text holds no words.
	Verb string `json:"verb"`
	// Object is what a DROP drops: table, schema, database, view,
	// materialized view, or the word that followed DROP.
	Object string `json:"object,omitempty"`
	// Targets are the tables (or schemas, or databases) an UPDATE, DELETE,
	// TRUNCATE or DROP names, as written, without quotes.
	Targets []string `json:"targets,omitempty"`
	// HasWhere says an UPDATE or DELETE has a WHERE of its own, outside any
	// parentheses.
	HasWhere bool `json:"hasWhere,omitempty"`
	// Writes says the statement changes rows: the main statement, or a
	// data-changing CTE inside a WITH.
	Writes bool `json:"writes,omitempty"`
	// Cascade says a DROP or TRUNCATE ends in CASCADE.
	Cascade bool `json:"cascade,omitempty"`
	// Names are Targets as identifiers, part by part (schema, name), for
	// building SQL that names them again. Not sent: the console has Targets.
	Names [][]string `json:"-"`
	// Alias is what a DELETE calls its table, when it gives it a name.
	Alias string `json:"-"`
	// Returning says the statement has a RETURNING list of its own.
	Returning bool `json:"-"`
	// IfExists says a DROP is DROP … IF EXISTS.
	IfExists bool `json:"-"`
}

// Guard refusals. Their codes sit in the SQLSTATE field of the answer, in a
// class Postgres never uses, so the console can tell them from Postgres's own.
const (
	GuardNoWhere   = "AR001" // UPDATE or DELETE of every row
	GuardDrop      = "AR002" // DROP TABLE / SCHEMA / VIEW, TRUNCATE
	GuardDropDB    = "AR003" // DROP DATABASE: never from a session
	GuardCheckOnly = "AR004" // a check of something that is not DML
)

// PGGuard is why a statement was stopped before it ran, and what would let
// it run. Confirm is the phrase the console asks the person to type; the
// agent compares it, which keeps a slip of the hand from confirming, not a
// determined person, who has the grants anyway.
type PGGuard struct {
	Code    string   `json:"code"`
	Verb    string   `json:"verb"`
	Object  string   `json:"object,omitempty"`
	Targets []string `json:"targets,omitempty"`
	Cascade bool     `json:"cascade,omitempty"`
	Confirm string   `json:"confirm,omitempty"`
	Reason  string   `json:"reason"`
}

// GuardSQL decides whether a statement may run as sent in database dbName.
// confirm is what the person typed, if anything. It returns nil to let it
// run.
func GuardSQL(c SQLClass, dbName, confirm string) *PGGuard {
	g := &PGGuard{Verb: c.Verb, Object: c.Object, Targets: c.Targets, Cascade: c.Cascade}
	switch c.Verb {
	case "update", "delete":
		if c.HasWhere || len(c.Targets) == 0 {
			return nil
		}
		g.Code = GuardNoWhere
		g.Confirm = dbName + "/" + strings.Join(c.Targets, ", ")
		g.Reason = "This " + strings.ToUpper(c.Verb) + " has no WHERE, so it changes every row of " +
			strings.Join(c.Targets, ", ") + "."
	case "truncate":
		if len(c.Targets) == 0 {
			return nil
		}
		g.Code = GuardDrop
		g.Confirm = dbName + "/" + strings.Join(c.Targets, ", ")
		g.Reason = "TRUNCATE empties " + strings.Join(c.Targets, ", ") + " and cannot be rolled back once committed."
	case "drop":
		switch c.Object {
		case "database":
			g.Code = GuardDropDB
			g.Reason = "A database is never dropped from a session. Remove it from its Settings, which keeps a backup first."
			return g
		case "table", "schema", "view", "materialized view":
			if len(c.Targets) == 0 {
				return nil
			}
			g.Code = GuardDrop
			g.Confirm = dbName + "/" + strings.Join(c.Targets, ", ")
			g.Reason = "DROP " + strings.ToUpper(c.Object) + " removes " + strings.Join(c.Targets, ", ") +
				" and everything in it."
		default:
			return nil
		}
	default:
		return nil
	}
	if confirm != "" && confirm == g.Confirm {
		return nil
	}
	return g
}

// sqlToken is a word (keyword or identifier) or one punctuation character.
type sqlToken struct {
	text   string // lower case for bare words; as written for quoted ones
	quoted bool
	punct  byte // ( ) , ; . * or 0 for a word
	depth  int  // parentheses around it
}

// lexSQL splits a statement into words and the punctuation that shapes it,
// dropping comments, string literals and dollar-quoted bodies.
func lexSQL(s string) []sqlToken {
	var out []sqlToken
	depth := 0
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			// Postgres block comments nest.
			n := 0
			for i < len(s) {
				if s[i] == '/' && i+1 < len(s) && s[i+1] == '*' {
					n++
					i += 2
					continue
				}
				if s[i] == '*' && i+1 < len(s) && s[i+1] == '/' {
					n--
					i += 2
					if n == 0 {
						break
					}
					continue
				}
				i++
			}
		case c == '\'':
			i = skipQuoted(s, i, '\'', i > 0 && (s[i-1] == 'e' || s[i-1] == 'E'))
		case c == '"' || c == '`':
			// "name" in Postgres, `name` in MySQL; Postgres has no use for
			// a backtick, so reading one as a quote changes nothing there.
			j := i + 1
			var b strings.Builder
			for j < len(s) {
				if s[j] == c {
					if j+1 < len(s) && s[j+1] == c {
						b.WriteByte(c)
						j += 2
						continue
					}
					break
				}
				b.WriteByte(s[j])
				j++
			}
			out = append(out, sqlToken{text: b.String(), quoted: true, depth: depth})
			i = j + 1
		case c == '$':
			// $tag$ ... $tag$, or a parameter like $1.
			j := i + 1
			for j < len(s) && (isIdentByte(s[j]) && !(s[j] >= '0' && s[j] <= '9' && j == i+1)) {
				j++
			}
			if j < len(s) && s[j] == '$' {
				tag := s[i : j+1]
				end := strings.Index(s[j+1:], tag)
				if end < 0 {
					return out
				}
				i = j + 1 + end + len(tag)
			} else {
				i = j
				for i < len(s) && s[i] >= '0' && s[i] <= '9' {
					i++
				}
			}
		case c == '(':
			out = append(out, sqlToken{punct: c, depth: depth})
			depth++
			i++
		case c == ')':
			if depth > 0 {
				depth--
			}
			out = append(out, sqlToken{punct: c, depth: depth})
			i++
		case c == ',' || c == ';' || c == '.' || c == '*':
			out = append(out, sqlToken{punct: c, depth: depth})
			i++
		case isIdentStart(s, i):
			j := i
			for j < len(s) {
				r, n := utf8.DecodeRuneInString(s[j:])
				if r == '_' || r == '$' || unicode.IsLetter(r) || unicode.IsDigit(r) {
					j += n
					continue
				}
				break
			}
			out = append(out, sqlToken{text: strings.ToLower(s[i:j]), depth: depth})
			i = j
		default:
			i++
		}
	}
	return out
}

// skipQuoted returns the index after a quoted literal starting at i.
func skipQuoted(s string, i int, q byte, backslash bool) int {
	j := i + 1
	for j < len(s) {
		if backslash && s[j] == '\\' {
			j += 2
			continue
		}
		if s[j] == q {
			if j+1 < len(s) && s[j+1] == q {
				j += 2
				continue
			}
			return j + 1
		}
		j++
	}
	return j
}

func isIdentByte(b byte) bool {
	return b == '_' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b >= 0x80
}

func isIdentStart(s string, i int) bool {
	r, _ := utf8.DecodeRuneInString(s[i:])
	return r == '_' || unicode.IsLetter(r)
}

// ClassifySQL reads a statement's shape. See SQLClass.
func ClassifySQL(sql string) SQLClass {
	toks := lexSQL(sql)
	// Only the first statement counts; the session runs one per request and
	// Postgres refuses the rest.
	for i, t := range toks {
		if t.punct == ';' && t.depth == 0 {
			toks = toks[:i]
			break
		}
	}
	var c SQLClass
	i := 0
	if i < len(toks) && toks[i].text == "with" && !toks[i].quoted {
		i = skipCTEs(toks, i+1, &c)
	}
	if i >= len(toks) || toks[i].punct != 0 {
		return c
	}
	c.Verb = toks[i].text
	rest := toks[i+1:]
	switch c.Verb {
	case "insert", "merge":
		c.Writes = true
	case "update":
		c.Writes = true
		j := skipWords(rest, 0, "only")
		name, _ := readName(rest, j)
		if name != "" {
			c.Targets = []string{name}
		}
		c.HasWhere = hasTopWord(rest, "where")
	case "delete":
		c.Writes = true
		j := skipWords(rest, 0, "from")
		j = skipWords(rest, j, "only")
		name, k := readName(rest, j)
		if name != "" {
			c.Targets = []string{name}
			parts, _ := readParts(rest, j)
			c.Names = [][]string{parts}
			if k < len(rest) && rest[k].punct == '*' {
				k++
			}
			k = skipWords(rest, k, "as")
			if k < len(rest) && rest[k].punct == 0 {
				switch w := rest[k]; {
				case w.quoted:
					c.Alias = w.text
				case w.text != "using" && w.text != "where" && w.text != "returning":
					c.Alias = w.text
				}
			}
		}
		c.HasWhere = hasTopWord(rest, "where")
		c.Returning = hasTopWord(rest, "returning")
	case "truncate":
		c.Writes = true
		j := skipWords(rest, 0, "table")
		c.Targets, c.Names = readNameList(rest, j)
		c.Cascade = lastWord(rest) == "cascade"
	case "drop":
		j := 0
		if j < len(rest) && rest[j].text == "materialized" && !rest[j].quoted {
			c.Object = "materialized view"
			j += 2
		} else if j < len(rest) && rest[j].punct == 0 {
			c.Object = rest[j].text
			j++
		}
		j = skipWords(rest, j, "concurrently")
		if j+1 < len(rest) && rest[j].text == "if" && rest[j+1].text == "exists" {
			j += 2
			c.IfExists = true
		}
		c.Targets, c.Names = readNameList(rest, j)
		c.Cascade = lastWord(rest) == "cascade"
	}
	return c
}

// skipCTEs moves past "[RECURSIVE] name [(cols)] AS [NOT] [MATERIALIZED]
// ( body ) [, ...]" and notes a CTE that changes rows.
func skipCTEs(toks []sqlToken, i int, c *SQLClass) int {
	i = skipWords(toks, i, "recursive")
	for i < len(toks) {
		// name, optional column list
		if toks[i].punct != 0 {
			return i
		}
		i++
		if i < len(toks) && toks[i].punct == '(' {
			i = closeParen(toks, i)
		}
		i = skipWords(toks, i, "as")
		i = skipWords(toks, i, "not")
		i = skipWords(toks, i, "materialized")
		if i >= len(toks) || toks[i].punct != '(' {
			return i
		}
		if i+1 < len(toks) {
			switch toks[i+1].text {
			case "insert", "update", "delete", "merge":
				c.Writes = true
			}
		}
		i = closeParen(toks, i)
		if i < len(toks) && toks[i].punct == ',' && toks[i].depth == 0 {
			i++
			continue
		}
		return i
	}
	return i
}

// closeParen returns the index after the ) that closes the ( at i.
func closeParen(toks []sqlToken, i int) int {
	d := toks[i].depth
	for j := i + 1; j < len(toks); j++ {
		if toks[j].punct == ')' && toks[j].depth == d {
			return j + 1
		}
	}
	return len(toks)
}

func skipWords(toks []sqlToken, i int, w string) int {
	if i < len(toks) && toks[i].punct == 0 && !toks[i].quoted && toks[i].text == w {
		return i + 1
	}
	return i
}

// readName reads name or schema.name at i.
func readName(toks []sqlToken, i int) (string, int) {
	if i >= len(toks) || toks[i].punct != 0 {
		return "", i
	}
	name := toks[i].text
	i++
	for i+1 < len(toks) && toks[i].punct == '.' && toks[i+1].punct == 0 {
		name += "." + toks[i+1].text
		i += 2
	}
	return name, i
}

// readParts reads name or schema.name at i as its parts.
func readParts(toks []sqlToken, i int) ([]string, int) {
	if i >= len(toks) || toks[i].punct != 0 {
		return nil, i
	}
	parts := []string{toks[i].text}
	i++
	for i+1 < len(toks) && toks[i].punct == '.' && toks[i+1].punct == 0 {
		parts = append(parts, toks[i+1].text)
		i += 2
	}
	return parts, i
}

// readNameList reads "a, b.c, d" at the top level, stopping at a keyword
// that ends it. It returns each name as written and as its parts.
func readNameList(toks []sqlToken, i int) ([]string, [][]string) {
	var names []string
	var parts [][]string
	for i < len(toks) {
		i = skipWords(toks, i, "only")
		name, j := readName(toks, i)
		if name == "" {
			break
		}
		if !toks[i].quoted {
			switch name {
			case "cascade", "restrict", "restart", "continue", "with":
				return names, parts
			}
		}
		p, _ := readParts(toks, i)
		names = append(names, name)
		parts = append(parts, p)
		i = j
		if i < len(toks) && toks[i].punct == '*' {
			i++
		}
		if i < len(toks) && toks[i].punct == ',' {
			i++
			continue
		}
		break
	}
	return names, parts
}

func hasTopWord(toks []sqlToken, w string) bool {
	for _, t := range toks {
		if t.depth == 0 && t.punct == 0 && !t.quoted && t.text == w {
			return true
		}
	}
	return false
}

func lastWord(toks []sqlToken) string {
	for i := len(toks) - 1; i >= 0; i-- {
		if toks[i].punct == 0 && toks[i].depth == 0 {
			if toks[i].quoted {
				return ""
			}
			return toks[i].text
		}
	}
	return ""
}
