package agent

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"

	"go.mongodb.org/mongo-driver/v2/bson"
)

/*
The MongoDB command line: what a person types, in the shape mongosh takes it,
turned into one database command for the agent to run.

	db.orders.find({ status: "paid" }).sort({ paid_at: -1 }).limit(20)
	db.getCollection("order items").countDocuments({})
	db.runCommand({ dbStats: 1 })
	{ "listCollections": 1 }

Only literals are read: objects, arrays, strings, numbers, true, false, null,
regular expressions, and ObjectId, ISODate, Date, NumberInt, NumberLong,
NumberDecimal, UUID and BinData. There is no JavaScript: no variables, no
functions, no loops. What runs is one command document, and that document is
what MongoDB checks against the person's roles.
*/

// mongoCall is one parsed line.
type mongoCall struct {
	// Cmd is the command document, first key the command's name.
	Cmd bson.D
	// Coll is the collection it acts on, if any.
	Coll string
	// Method is the shell method typed ("find", "runCommand"), for the tag.
	Method string
	// Confirm says why the command waits for the server's name to be typed.
	Confirm string
	// Refuse says why the agent will not send it at all.
	Refuse string
	// Limit is the most documents the person asked for; 0 is no limit.
	Limit int64
}

// maxShellDepth bounds how deeply literals nest.
const maxShellDepth = 100

type shellParser struct {
	s     []rune
	i     int
	depth int
}

func (p *shellParser) errf(format string, a ...any) error {
	return fmt.Errorf("at character %d: %s", p.i+1, fmt.Sprintf(format, a...))
}

func (p *shellParser) space() {
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch {
		case unicode.IsSpace(c):
			p.i++
		case c == '/' && p.i+1 < len(p.s) && p.s[p.i+1] == '/':
			for p.i < len(p.s) && p.s[p.i] != '\n' {
				p.i++
			}
		case c == '/' && p.i+1 < len(p.s) && p.s[p.i+1] == '*':
			j := p.i + 2
			for j+1 < len(p.s) && (p.s[j] != '*' || p.s[j+1] != '/') {
				j++
			}
			p.i = min(j+2, len(p.s))
		default:
			return
		}
	}
}

func (p *shellParser) peek() rune {
	p.space()
	if p.i >= len(p.s) {
		return 0
	}
	return p.s[p.i]
}

func (p *shellParser) eat(c rune) bool {
	if p.peek() == c {
		p.i++
		return true
	}
	return false
}

func (p *shellParser) want(c rune) error {
	if !p.eat(c) {
		if p.i >= len(p.s) {
			return p.errf("expected %q, the line ended", c)
		}
		return p.errf("expected %q, found %q", c, p.s[p.i])
	}
	return nil
}

func identStart(c rune) bool { return c == '_' || c == '$' || unicode.IsLetter(c) }
func identPart(c rune) bool  { return identStart(c) || unicode.IsDigit(c) }

func (p *shellParser) ident() string {
	p.space()
	start := p.i
	if p.i < len(p.s) && identStart(p.s[p.i]) {
		p.i++
		for p.i < len(p.s) && identPart(p.s[p.i]) {
			p.i++
		}
	}
	return string(p.s[start:p.i])
}

func (p *shellParser) str() (string, error) {
	q := p.peek()
	if q != '"' && q != '\'' {
		return "", p.errf("expected a string")
	}
	p.i++
	var b strings.Builder
	for p.i < len(p.s) {
		c := p.s[p.i]
		p.i++
		switch {
		case c == q:
			return b.String(), nil
		case c == '\\':
			if p.i >= len(p.s) {
				return "", p.errf("the string is not closed")
			}
			e := p.s[p.i]
			p.i++
			switch e {
			case 'n':
				b.WriteRune('\n')
			case 't':
				b.WriteRune('\t')
			case 'r':
				b.WriteRune('\r')
			case 'b':
				b.WriteRune('\b')
			case 'f':
				b.WriteRune('\f')
			case '0':
				b.WriteRune(0)
			case 'u':
				if p.i+4 > len(p.s) {
					return "", p.errf("a \\u escape needs four hex digits")
				}
				n, err := strconv.ParseUint(string(p.s[p.i:p.i+4]), 16, 32)
				if err != nil {
					return "", p.errf("a \\u escape needs four hex digits")
				}
				p.i += 4
				b.WriteRune(rune(n))
			default:
				b.WriteRune(e)
			}
		default:
			b.WriteRune(c)
		}
	}
	return "", p.errf("the string is not closed")
}

func (p *shellParser) number() (any, error) {
	p.space()
	start := p.i
	if p.i < len(p.s) && (p.s[p.i] == '-' || p.s[p.i] == '+') {
		p.i++
	}
	if strings.HasPrefix(string(p.s[p.i:]), "Infinity") {
		p.i += len("Infinity")
		if p.s[start] == '-' {
			return math.Inf(-1), nil
		}
		return math.Inf(1), nil
	}
	float := false
	for p.i < len(p.s) {
		c := p.s[p.i]
		if unicode.IsDigit(c) {
			p.i++
			continue
		}
		if c == '.' || c == 'e' || c == 'E' || ((c == '-' || c == '+') && (p.s[p.i-1] == 'e' || p.s[p.i-1] == 'E')) {
			float = true
			p.i++
			continue
		}
		break
	}
	text := string(p.s[start:p.i])
	if !float {
		if n, err := strconv.ParseInt(text, 10, 64); err == nil {
			if n >= math.MinInt32 && n <= math.MaxInt32 {
				return int32(n), nil
			}
			return n, nil
		}
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return nil, p.errf("%q is not a number", text)
	}
	return f, nil
}

func (p *shellParser) value() (any, error) {
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > maxShellDepth {
		return nil, p.errf("nested too deeply")
	}
	switch c := p.peek(); {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '"' || c == '\'':
		return p.str()
	case c == '/':
		return p.regex()
	case c == '-' || c == '+' || c == '.' || unicode.IsDigit(c):
		return p.number()
	case identStart(c):
		return p.word()
	case c == 0:
		return nil, p.errf("expected a value, the line ended")
	default:
		return nil, p.errf("expected a value, found %q", c)
	}
}

func (p *shellParser) object() (bson.D, error) {
	if err := p.want('{'); err != nil {
		return nil, err
	}
	d := bson.D{}
	for !p.eat('}') {
		var key string
		switch c := p.peek(); {
		case c == '"' || c == '\'':
			k, err := p.str()
			if err != nil {
				return nil, err
			}
			key = k
		case identStart(c):
			key = p.ident()
		case unicode.IsDigit(c):
			start := p.i
			for p.i < len(p.s) && unicode.IsDigit(p.s[p.i]) {
				p.i++
			}
			key = string(p.s[start:p.i])
		default:
			return nil, p.errf("expected a field name")
		}
		if err := p.want(':'); err != nil {
			return nil, err
		}
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		d = append(d, bson.E{Key: key, Value: v})
		if !p.eat(',') {
			if err := p.want('}'); err != nil {
				return nil, err
			}
			break
		}
	}
	return d, nil
}

func (p *shellParser) array() (bson.A, error) {
	if err := p.want('['); err != nil {
		return nil, err
	}
	a := bson.A{}
	for !p.eat(']') {
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		a = append(a, v)
		if !p.eat(',') {
			if err := p.want(']'); err != nil {
				return nil, err
			}
			break
		}
	}
	return a, nil
}

func (p *shellParser) regex() (any, error) {
	p.i++ // the opening /
	var b strings.Builder
	inClass := false
	for {
		if p.i >= len(p.s) {
			return nil, p.errf("the regular expression is not closed")
		}
		c := p.s[p.i]
		p.i++
		if c == '\\' && p.i < len(p.s) {
			b.WriteRune(c)
			b.WriteRune(p.s[p.i])
			p.i++
			continue
		}
		if c == '[' {
			inClass = true
		} else if c == ']' {
			inClass = false
		} else if c == '/' && !inClass {
			break
		}
		b.WriteRune(c)
	}
	start := p.i
	for p.i < len(p.s) && strings.ContainsRune("imsxu", p.s[p.i]) {
		p.i++
	}
	return bson.Regex{Pattern: b.String(), Options: string(p.s[start:p.i])}, nil
}

// args reads a parenthesised list of values.
func (p *shellParser) args() ([]any, error) {
	if err := p.want('('); err != nil {
		return nil, err
	}
	var out []any
	for !p.eat(')') {
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		if !p.eat(',') {
			if err := p.want(')'); err != nil {
				return nil, err
			}
			break
		}
	}
	return out, nil
}

func (p *shellParser) strArg() (string, error) {
	if err := p.want('('); err != nil {
		return "", err
	}
	s, err := p.str()
	if err != nil {
		return "", err
	}
	return s, p.want(')')
}

// word is a keyword or a constructor: ObjectId("…") and the like.
func (p *shellParser) word() (any, error) {
	w := p.ident()
	if w == "new" {
		w = p.ident()
	}
	switch w {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "null", "undefined":
		return nil, nil
	case "Infinity":
		return math.Inf(1), nil
	case "NaN":
		return math.NaN(), nil
	case "ObjectId":
		if p.peekAfterParen() {
			if err := p.want('('); err != nil {
				return nil, err
			}
			if err := p.want(')'); err != nil {
				return nil, err
			}
			return bson.NewObjectID(), nil
		}
		s, err := p.strArg()
		if err != nil {
			return nil, err
		}
		id, err := bson.ObjectIDFromHex(s)
		if err != nil {
			return nil, p.errf("ObjectId(%q) is not 24 hex digits", s)
		}
		return id, nil
	case "ISODate", "Date":
		if p.peekAfterParen() {
			if err := p.want('('); err != nil {
				return nil, err
			}
			if err := p.want(')'); err != nil {
				return nil, err
			}
			return bson.NewDateTimeFromTime(time.Now()), nil
		}
		if err := p.want('('); err != nil {
			return nil, err
		}
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		if err := p.want(')'); err != nil {
			return nil, err
		}
		switch t := v.(type) {
		case string:
			tm, err := parseShellTime(t)
			if err != nil {
				return nil, p.errf("%s(%q): %v", w, t, err)
			}
			return bson.NewDateTimeFromTime(tm), nil
		case int32:
			return bson.DateTime(int64(t)), nil
		case int64:
			return bson.DateTime(t), nil
		case float64:
			return bson.DateTime(int64(t)), nil
		}
		return nil, p.errf("%s takes a date string or milliseconds", w)
	case "NumberInt", "NumberLong", "NumberDecimal":
		if err := p.want('('); err != nil {
			return nil, err
		}
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		if err := p.want(')'); err != nil {
			return nil, err
		}
		text := fmt.Sprint(v)
		if s, ok := v.(string); ok {
			text = s
		}
		switch w {
		case "NumberInt":
			n, err := strconv.ParseInt(text, 10, 32)
			if err != nil {
				return nil, p.errf("NumberInt(%s) is not a 32-bit integer", text)
			}
			return int32(n), nil
		case "NumberLong":
			n, err := strconv.ParseInt(text, 10, 64)
			if err != nil {
				return nil, p.errf("NumberLong(%s) is not a 64-bit integer", text)
			}
			return n, nil
		default:
			d, err := bson.ParseDecimal128(text)
			if err != nil {
				return nil, p.errf("NumberDecimal(%s) is not a decimal", text)
			}
			return d, nil
		}
	case "UUID":
		s, err := p.strArg()
		if err != nil {
			return nil, err
		}
		b, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
		if err != nil || len(b) != 16 {
			return nil, p.errf("UUID(%q) is not a UUID", s)
		}
		return bson.Binary{Subtype: 4, Data: b}, nil
	case "BinData":
		a, err := p.args()
		if err != nil {
			return nil, err
		}
		if len(a) != 2 {
			return nil, p.errf("BinData takes a subtype and base64")
		}
		sub, ok1 := a[0].(int32)
		text, ok2 := a[1].(string)
		data, err := base64.StdEncoding.DecodeString(text)
		if !ok1 || !ok2 || err != nil || sub < 0 || sub > 255 {
			return nil, p.errf("BinData takes a subtype and base64")
		}
		return bson.Binary{Subtype: byte(sub), Data: data}, nil
	case "":
		return nil, p.errf("expected a value")
	}
	return nil, p.errf("%s is not something the command line knows; only literal values are read", w)
}

// peekAfterParen reports whether the next two tokens are "(" and ")".
func (p *shellParser) peekAfterParen() bool {
	save := p.i
	defer func() { p.i = save }()
	return p.eat('(') && p.eat(')')
}

func parseShellTime(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05Z0700",
		"2006-01-02 15:04:05", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("not a date MongoDB's shell would read")
}

// parseMongoLine reads one line of the command line.
func parseMongoLine(line string) (mongoCall, error) {
	line = strings.TrimSpace(line)
	line = strings.TrimSpace(strings.TrimSuffix(line, ";"))
	p := &shellParser{s: []rune(line)}
	if line == "" {
		return mongoCall{}, errors.New("nothing to run")
	}
	var call mongoCall
	var err error
	switch {
	case p.peek() == '{':
		var d bson.D
		d, err = p.object()
		call = mongoCall{Cmd: d, Method: "runCommand"}
	case strings.EqualFold(line, "show collections") || strings.EqualFold(line, "show tables"):
		return mongoCall{Cmd: bson.D{{Key: "listCollections", Value: 1}, {Key: "nameOnly", Value: true}}, Method: "show collections"}, nil
	default:
		call, err = p.dbCall()
	}
	if err != nil {
		return mongoCall{}, err
	}
	if p.peek() != 0 {
		return mongoCall{}, p.errf("unexpected %q after the command", p.s[p.i])
	}
	if len(call.Cmd) == 0 {
		return mongoCall{}, errors.New("the command is empty")
	}
	classifyMongo(&call)
	return call, nil
}

// dbCall reads db.<collection>.<method>(…)[.<modifier>(…)]… or db.<method>(…).
func (p *shellParser) dbCall() (mongoCall, error) {
	if p.ident() != "db" {
		return mongoCall{}, p.errf("start with db. (db.orders.find({})) or a command document ({ ping: 1 })")
	}
	var coll string
	switch {
	case p.eat('['):
		s, err := p.str()
		if err != nil {
			return mongoCall{}, err
		}
		if err := p.want(']'); err != nil {
			return mongoCall{}, err
		}
		coll = s
	case p.eat('.'):
		name := p.ident()
		if name == "" {
			return mongoCall{}, p.errf("expected a collection or a method after db.")
		}
		if p.peek() == '(' {
			return p.dbMethod(name)
		}
		if name == "getCollection" {
			return mongoCall{}, p.errf("getCollection takes the collection's name")
		}
		coll = name
	default:
		return mongoCall{}, p.errf("expected . after db")
	}
	if err := p.want('.'); err != nil {
		return mongoCall{}, err
	}
	return p.collMethod(coll)
}

func (p *shellParser) dbMethod(name string) (mongoCall, error) {
	if name == "getCollection" {
		s, err := p.strArg()
		if err != nil {
			return mongoCall{}, err
		}
		if err := p.want('.'); err != nil {
			return mongoCall{}, err
		}
		return p.collMethod(s)
	}
	args, err := p.args()
	if err != nil {
		return mongoCall{}, err
	}
	c := mongoCall{Method: name}
	switch name {
	case "runCommand", "adminCommand":
		if name == "adminCommand" {
			return mongoCall{}, errors.New("adminCommand runs on the admin database; this entry is one database, so use runCommand")
		}
		if len(args) != 1 {
			return mongoCall{}, errors.New("runCommand takes one command document")
		}
		switch d := args[0].(type) {
		case bson.D:
			c.Cmd = d
		case string:
			c.Cmd = bson.D{{Key: d, Value: 1}}
		default:
			return mongoCall{}, errors.New("runCommand takes one command document")
		}
	case "getCollectionNames", "getCollectionInfos":
		c.Cmd = bson.D{{Key: "listCollections", Value: 1}, {Key: "nameOnly", Value: name == "getCollectionNames"}}
		if len(args) > 0 {
			c.Cmd = append(c.Cmd, bson.E{Key: "filter", Value: args[0]})
		}
	case "stats":
		c.Cmd = bson.D{{Key: "dbStats", Value: 1}}
	case "createCollection":
		if len(args) < 1 {
			return mongoCall{}, errors.New("createCollection takes the collection's name")
		}
		n, ok := args[0].(string)
		if !ok {
			return mongoCall{}, errors.New("createCollection takes the collection's name")
		}
		c.Cmd = bson.D{{Key: "create", Value: n}}
		if len(args) > 1 {
			if o, ok := args[1].(bson.D); ok {
				c.Cmd = append(c.Cmd, o...)
			}
		}
	case "dropDatabase":
		c.Cmd = bson.D{{Key: "dropDatabase", Value: 1}}
	default:
		return mongoCall{}, fmt.Errorf("db.%s is not something the command line runs; db.runCommand({...}) runs any command", name)
	}
	return c, nil
}

func docArg(args []any, i int) (any, bool) {
	if i >= len(args) {
		return bson.D{}, true
	}
	switch args[i].(type) {
	case bson.D:
		return args[i], true
	case nil:
		return bson.D{}, true
	}
	return nil, false
}

func (p *shellParser) collMethod(coll string) (mongoCall, error) {
	if coll == "" {
		return mongoCall{}, errors.New("the collection has no name")
	}
	method := p.ident()
	if method == "" {
		return mongoCall{}, p.errf("expected a method, such as find")
	}
	args, err := p.args()
	if err != nil {
		return mongoCall{}, err
	}
	c := mongoCall{Coll: coll, Method: method}
	bad := func(what string) (mongoCall, error) {
		return mongoCall{}, fmt.Errorf("%s takes %s", method, what)
	}
	switch method {
	case "find", "findOne":
		f, ok1 := docArg(args, 0)
		proj, ok2 := docArg(args, 1)
		if !ok1 || !ok2 || len(args) > 2 {
			return bad("a filter and a projection")
		}
		c.Cmd = bson.D{{Key: "find", Value: coll}, {Key: "filter", Value: f}}
		if len(args) > 1 {
			c.Cmd = append(c.Cmd, bson.E{Key: "projection", Value: proj})
		}
		if method == "findOne" {
			c.Limit = 1
		}
		for p.eat('.') {
			mod := p.ident()
			margs, err := p.args()
			if err != nil {
				return mongoCall{}, err
			}
			if method == "findOne" {
				return mongoCall{}, fmt.Errorf("findOne cannot be followed by .%s", mod)
			}
			switch mod {
			case "limit", "skip":
				n, ok := asInt(margs)
				if !ok || n < 0 {
					return mongoCall{}, fmt.Errorf(".%s takes a number", mod)
				}
				if mod == "limit" {
					c.Limit = n
				} else {
					c.Cmd = append(c.Cmd, bson.E{Key: "skip", Value: n})
				}
			case "sort", "projection", "hint", "collation":
				if len(margs) != 1 {
					return mongoCall{}, fmt.Errorf(".%s takes one document", mod)
				}
				c.Cmd = append(c.Cmd, bson.E{Key: mod, Value: margs[0]})
			case "count":
				c.Cmd = bson.D{{Key: "count", Value: coll}, {Key: "query", Value: f}}
				c.Method = "count"
			default:
				return mongoCall{}, fmt.Errorf(".%s is not something the command line runs after find", mod)
			}
		}
		if c.Limit > 0 && c.Method != "count" {
			c.Cmd = append(c.Cmd, bson.E{Key: "limit", Value: c.Limit})
		}
		return c, nil
	case "countDocuments":
		f, ok := docArg(args, 0)
		if !ok || len(args) > 1 {
			return bad("a filter")
		}
		c.Cmd = bson.D{{Key: "count", Value: coll}, {Key: "query", Value: f}}
	case "estimatedDocumentCount":
		c.Cmd = bson.D{{Key: "count", Value: coll}}
	case "distinct":
		if len(args) < 1 {
			return bad("a field name and a filter")
		}
		key, ok := args[0].(string)
		f, ok2 := docArg(args, 1)
		if !ok || !ok2 || len(args) > 2 {
			return bad("a field name and a filter")
		}
		c.Cmd = bson.D{{Key: "distinct", Value: coll}, {Key: "key", Value: key}, {Key: "query", Value: f}}
	case "aggregate":
		pipe := bson.A{}
		if len(args) > 0 {
			a, ok := args[0].(bson.A)
			if !ok {
				return bad("a pipeline: a list of stages")
			}
			pipe = a
		}
		c.Cmd = bson.D{{Key: "aggregate", Value: coll}, {Key: "pipeline", Value: pipe}, {Key: "cursor", Value: bson.D{}}}
		if len(args) > 1 {
			if o, ok := args[1].(bson.D); ok {
				c.Cmd = append(c.Cmd, o...)
			}
		}
	case "insertOne":
		if len(args) < 1 {
			return bad("a document")
		}
		d, ok := args[0].(bson.D)
		if !ok {
			return bad("a document")
		}
		c.Cmd = bson.D{{Key: "insert", Value: coll}, {Key: "documents", Value: bson.A{d}}}
	case "insertMany":
		if len(args) < 1 {
			return bad("a list of documents")
		}
		a, ok := args[0].(bson.A)
		if !ok || len(a) == 0 {
			return bad("a list of documents")
		}
		c.Cmd = bson.D{{Key: "insert", Value: coll}, {Key: "documents", Value: a}}
	case "updateOne", "updateMany", "replaceOne":
		if len(args) < 2 {
			return bad("a filter and an update")
		}
		f, ok1 := args[0].(bson.D)
		u := args[1]
		_, isDoc := u.(bson.D)
		_, isPipe := u.(bson.A)
		if !ok1 || !(isDoc || (isPipe && method != "replaceOne")) {
			return bad("a filter and an update")
		}
		up := bson.D{{Key: "q", Value: f}, {Key: "u", Value: u}, {Key: "multi", Value: method == "updateMany"}}
		if len(args) > 2 {
			if o, ok := args[2].(bson.D); ok {
				for _, e := range o {
					if e.Key == "upsert" || e.Key == "arrayFilters" || e.Key == "collation" || e.Key == "hint" {
						up = append(up, e)
					}
				}
			}
		}
		c.Cmd = bson.D{{Key: "update", Value: coll}, {Key: "updates", Value: bson.A{up}}}
	case "deleteOne", "deleteMany":
		if len(args) < 1 {
			return bad("a filter; {} matches every document")
		}
		f, ok := args[0].(bson.D)
		if !ok {
			return bad("a filter; {} matches every document")
		}
		lim := int32(0)
		if method == "deleteOne" {
			lim = 1
		}
		c.Cmd = bson.D{{Key: "delete", Value: coll}, {Key: "deletes", Value: bson.A{bson.D{{Key: "q", Value: f}, {Key: "limit", Value: lim}}}}}
	case "drop":
		c.Cmd = bson.D{{Key: "drop", Value: coll}}
	case "createIndex":
		if len(args) < 1 {
			return bad("the index's keys")
		}
		keys, ok := args[0].(bson.D)
		if !ok || len(keys) == 0 {
			return bad("the index's keys")
		}
		idx := bson.D{{Key: "key", Value: keys}}
		var opts bson.D
		if len(args) > 1 {
			opts, _ = args[1].(bson.D)
		}
		name := ""
		for _, e := range opts {
			if e.Key == "name" {
				name, _ = e.Value.(string)
			}
		}
		if name == "" {
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				parts = append(parts, k.Key+"_"+fmt.Sprint(k.Value))
			}
			name = strings.Join(parts, "_")
		}
		idx = append(idx, bson.E{Key: "name", Value: name})
		for _, e := range opts {
			if e.Key != "name" {
				idx = append(idx, e)
			}
		}
		c.Cmd = bson.D{{Key: "createIndexes", Value: coll}, {Key: "indexes", Value: bson.A{idx}}}
	case "dropIndex":
		if len(args) != 1 {
			return bad("the index's name or keys")
		}
		c.Cmd = bson.D{{Key: "dropIndexes", Value: coll}, {Key: "index", Value: args[0]}}
	case "getIndexes":
		c.Cmd = bson.D{{Key: "listIndexes", Value: coll}}
	case "stats":
		c.Cmd = bson.D{{Key: "aggregate", Value: coll}, {Key: "pipeline", Value: bson.A{bson.D{{Key: "$collStats", Value: bson.D{{Key: "storageStats", Value: bson.D{}}, {Key: "count", Value: bson.D{}}}}}}}, {Key: "cursor", Value: bson.D{}}}
	default:
		return mongoCall{}, fmt.Errorf("%s is not something the command line runs on a collection; db.runCommand({...}) runs any command", method)
	}
	if p.peek() == '.' {
		return mongoCall{}, p.errf("%s cannot be followed by more methods", method)
	}
	return c, nil
}

func asInt(a []any) (int64, bool) {
	if len(a) != 1 {
		return 0, false
	}
	switch n := a[0].(type) {
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		if n == math.Trunc(n) {
			return int64(n), true
		}
	}
	return 0, false
}

// classifyMongo marks what waits for the server's name and what is never
// sent. MongoDB's roles decide everything else; these are the agent's own
// rules on top: wiping a collection or the database is confirmed, and the
// users Argus manages are left to Argus.
func classifyMongo(c *mongoCall) {
	if len(c.Cmd) == 0 {
		return
	}
	name := c.Cmd[0].Key
	switch strings.ToLower(name) {
	case "dropdatabase":
		c.Confirm = "dropping the database"
	case "drop":
		c.Confirm = "dropping a collection"
	case "delete":
		if ds, ok := field(c.Cmd, "deletes").(bson.A); ok {
			for _, d := range ds {
				if dd, ok := d.(bson.D); ok {
					q, _ := field(dd, "q").(bson.D)
					lim := fmt.Sprint(field(dd, "limit"))
					if len(q) == 0 && lim == "0" {
						c.Confirm = "deleting every document in a collection"
					}
				}
			}
		}
	case "update":
		if us, ok := field(c.Cmd, "updates").(bson.A); ok {
			for _, u := range us {
				if ud, ok := u.(bson.D); ok {
					q, _ := field(ud, "q").(bson.D)
					if len(q) == 0 && field(ud, "multi") == true {
						c.Confirm = "changing every document in a collection"
					}
				}
			}
		}
	case "dropallusersfromdatabase":
		c.Refuse = "it would remove the users Argus gives people; Argus manages argus- users itself"
	case "createuser", "updateuser", "dropuser", "grantrolestouser", "revokerolesfromuser":
		if u, _ := c.Cmd[0].Value.(string); strings.HasPrefix(u, "argus-") {
			c.Refuse = u + " is a user Argus manages; it is not changed by hand"
		}
	}
}

func field(d bson.D, key string) any {
	for _, e := range d {
		if e.Key == key {
			return e.Value
		}
	}
	return nil
}
