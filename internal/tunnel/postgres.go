package tunnel

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

/*
The postgres service's frames, after the header and answer.

The same framing as the terminal and files: type, length, payload.

	toward the agent     'q' PGRequest (JSON)
	toward the plane     'a' PGAnswer (JSON): cols, done, error, schema, ...
	                     'r' a batch of rows (binary, below), relayed to the
	                         browser untouched
	                     'l' PGRecord (JSON): one line of the statement
	                         recording, sent before the statement runs

A row batch is the request's id (uint32, big-endian), then rows one after
another, each cell an unsigned varint and its bytes:

	0          NULL
	n > 0      (n-1)>>1 bytes of text follow; (n-1)&1 says the value was cut
	           at MaxCell

The browser knows how many cells a row has from the "cols" answer. Values are
Postgres's own text format, so the agent decodes nothing and the browser shows
exactly what Postgres printed.
*/

// Postgres frame types.
const (
	FramePGRequest byte = 'q'
	FramePGAnswer  byte = 'a'
	FramePGRows    byte = 'r'
	FramePGRecord  byte = 'l'
)

// PostgresService is the tunnel service name.
const PostgresService = "postgres"

// Limits on what one statement may move. MaxRowsCeiling is the most rows a
// person can ask to see; the database's own policy may set it lower.
const (
	// MaxCell is the longest value sent in full. A cell is for reading: a
	// larger one is cut and marked, and a person who needs it whole can
	// select a substring of it.
	MaxCell = 64 << 10
	// RowBatchBytes is the size a batch is flushed at.
	RowBatchBytes = 128 << 10
	// RowBatchRows is the most rows in one batch, so the first screen of a
	// fast query paints before the rest has been read.
	RowBatchRows = 500
	// MaxRowsCeiling bounds MaxRows whatever a request asks for.
	MaxRowsCeiling = 100_000
	// DefaultMaxRows is what a request that names no limit gets.
	DefaultMaxRows = 1_000
	// MaxSQL bounds the text of one statement.
	MaxSQL = 1 << 20
)

// PGRequest is one thing the browser asks of a session.
type PGRequest struct {
	// ID ties answers and rows to the request. The browser numbers them.
	ID uint32 `json:"id"`
	// Op is run, cancel or schema.
	Op string `json:"op"`
	// SQL is one statement, for run.
	SQL string `json:"sql,omitempty"`
	// MaxRows is how many rows run sends before it stops the statement.
	MaxRows int `json:"maxRows,omitempty"`
	// Kind and Schema are what schema lists: "schemas", "objects" (in
	// Schema) or "columns" (of Object in Schema).
	Kind   string `json:"kind,omitempty"`
	Schema string `json:"schema,omitempty"`
	Object string `json:"object,omitempty"`
}

// PGColumn is one column of a result.
type PGColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// PGObject is one item in the object tree.
type PGObject struct {
	Name string `json:"name"`
	// Kind is schema, table, view, matview, function, sequence or column.
	Kind string `json:"kind"`
	// Detail is the column's type, a function's arguments, a schema's owner.
	Detail string `json:"detail,omitempty"`
	// Count is how many tables a schema holds, or a table's estimated rows.
	Count int64 `json:"count,omitempty"`
}

// PGAnswer is the agent's reply to a request, or news about the session.
type PGAnswer struct {
	ID uint32 `json:"id"`
	// Type is ready, cols, done, error or schema.
	Type string     `json:"type"`
	Cols []PGColumn `json:"cols,omitempty"`
	// Tag is Postgres's command tag: "SELECT 214", "UPDATE 1".
	Tag  string `json:"tag,omitempty"`
	Rows int64  `json:"rows,omitempty"`
	MS   int64  `json:"ms,omitempty"`
	// Truncated says the statement was stopped at MaxRows.
	Truncated bool `json:"truncated,omitempty"`
	// Error, Code (SQLSTATE) and Position (1-based, in characters) describe
	// a failure the way Postgres reported it.
	Error    string `json:"error,omitempty"`
	Code     string `json:"code,omitempty"`
	Position int    `json:"position,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Hint     string `json:"hint,omitempty"`
	// Objects answers schema.
	Objects []PGObject `json:"objects,omitempty"`
	// Ready carries what the session opened as.
	Role     string `json:"role,omitempty"`
	Level    string `json:"level,omitempty"`
	Version  string `json:"version,omitempty"`
	Session  string `json:"session,omitempty"`
	ReadOnly bool   `json:"readOnly,omitempty"`
}

// PGRecord is one line of a database session's recording. Each line carries
// the hash of the line before it, so a line removed or changed breaks every
// hash after it.
type PGRecord struct {
	Seq  int64  `json:"seq"`
	Time string `json:"t"`
	// Kind is open, stmt, done, error, cancel or close.
	Kind string `json:"kind"`
	// SQL is the statement, on stmt.
	SQL string `json:"sql,omitempty"`
	// Request ties stmt, done and error together.
	Request uint32 `json:"req,omitempty"`
	Tag     string `json:"tag,omitempty"`
	Rows    int64  `json:"rows,omitempty"`
	MS      int64  `json:"ms,omitempty"`
	// ResultSHA256 is the hash of the rows exactly as they were sent.
	ResultSHA256 string `json:"result,omitempty"`
	Code         string `json:"code,omitempty"`
	Error        string `json:"error,omitempty"`
	Truncated    bool   `json:"truncated,omitempty"`
	// Actor, Role and Level are set on open.
	Actor string `json:"actor,omitempty"`
	Role  string `json:"role,omitempty"`
	Level string `json:"level,omitempty"`
	Grant string `json:"grant,omitempty"`
	// Prev is the previous line's hash; Hash is this line's, over the line
	// with Hash empty.
	Prev string `json:"prev"`
	Hash string `json:"hash,omitempty"`
}

// GrantDB is what a postgres grant opens. Every field is signed, so the
// agent connects where the control plane's database says and nowhere a relay
// would prefer.
type GrantDB struct {
	// ID is the database's id in the control plane.
	ID string `json:"id"`
	// Name is the database to connect to.
	Name string `json:"name"`
	// Host is 127.0.0.1, ::1, localhost, or an absolute socket directory.
	Host string `json:"host"`
	Port int    `json:"port"`
	// Level is read, write, owner or dba.
	Level string `json:"level"`
	// Policy is what the session must enforce.
	StatementTimeoutMS int `json:"stmtTimeoutMs,omitempty"`
	IdleTxTimeoutMS    int `json:"idleTxTimeoutMs,omitempty"`
	MaxRows            int `json:"maxRows,omitempty"`
	IdleTimeoutS       int `json:"idleTimeoutS,omitempty"`
	MaxSessionS        int `json:"maxSessionS,omitempty"`
}

// Postgres access levels, least to most.
const (
	LevelRead  = "read"
	LevelWrite = "write"
	LevelOwner = "owner"
	LevelDBA   = "dba"
)

// LevelRank orders levels; zero is not a level.
func LevelRank(level string) int {
	switch level {
	case LevelRead:
		return 1
	case LevelWrite:
		return 2
	case LevelOwner:
		return 3
	case LevelDBA:
		return 4
	}
	return 0
}

var roleName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// ValidPersonalRole reports whether name may be a person's role: a plain,
// unquoted identifier that is not one of Postgres's or Argus's own.
func ValidPersonalRole(name string) bool {
	if !roleName.MatchString(name) {
		return false
	}
	for _, p := range []string{"pg_", "argus_"} {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	switch name {
	case "postgres", "public", "current_user", "session_user", "current_role", "none", "all":
		return false
	}
	return true
}

// PersonalRole turns a login name into a role name: lowercased, anything
// outside [a-z0-9_] replaced by an underscore. Empty when nothing usable is
// left.
func PersonalRole(login string) string {
	login = strings.ToLower(login)
	if i := strings.IndexByte(login, '@'); i >= 0 {
		login = login[:i]
	}
	b := make([]byte, 0, len(login))
	for i := 0; i < len(login) && len(b) < 63; i++ {
		c := login[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' {
			b = append(b, c)
		} else {
			b = append(b, '_')
		}
	}
	name := string(b)
	if strings.Trim(name, "_") == "" {
		return ""
	}
	if name[0] >= '0' && name[0] <= '9' {
		name = "u" + name
		if len(name) > 63 {
			name = name[:63]
		}
	}
	if !ValidPersonalRole(name) {
		name = "u_" + name
		if len(name) > 63 {
			name = name[:63]
		}
	}
	if !ValidPersonalRole(name) {
		return ""
	}
	return name
}

// LocalPGHost reports whether host is somewhere on this machine. An agent
// only ever connects to its own server's Postgres, so a grant cannot turn it
// into a way to reach the rest of the network.
func LocalPGHost(host string) bool {
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return strings.HasPrefix(host, "/") && !strings.Contains(host, "..")
}

// RowEncoder builds row batches. The zero value is not ready; use
// NewRowEncoder. It reuses its buffer between batches.
type RowEncoder struct {
	buf  []byte
	rows int
	tmp  [binary.MaxVarintLen64]byte
}

// NewRowEncoder starts batches for request id.
func NewRowEncoder(id uint32) *RowEncoder {
	e := &RowEncoder{buf: make([]byte, 4, RowBatchBytes+MaxCell+64)}
	binary.BigEndian.PutUint32(e.buf, id)
	return e
}

// Add appends one row. A nil cell is NULL.
func (e *RowEncoder) Add(cells [][]byte) {
	for _, c := range cells {
		if c == nil {
			e.buf = append(e.buf, 0)
			continue
		}
		cut := uint64(0)
		if len(c) > MaxCell {
			c = trimUTF8(c[:MaxCell])
			cut = 1
		}
		n := binary.PutUvarint(e.tmp[:], (uint64(len(c))<<1|cut)+1)
		e.buf = append(e.buf, e.tmp[:n]...)
		e.buf = append(e.buf, c...)
	}
	e.rows++
}

// Full reports whether the batch should be sent now.
func (e *RowEncoder) Full() bool { return e.rows >= RowBatchRows || len(e.buf) >= RowBatchBytes }

// Rows is how many rows the batch holds.
func (e *RowEncoder) Rows() int { return e.rows }

// Batch is the frame payload. Valid until Reset.
func (e *RowEncoder) Batch() []byte { return e.buf }

// Reset empties the batch, keeping the id and the buffer.
func (e *RowEncoder) Reset() { e.buf = e.buf[:4]; e.rows = 0 }

// trimUTF8 drops a partial UTF-8 sequence at the end of b, so a cut value is
// still text.
func trimUTF8(b []byte) []byte {
	for i := 0; i < 4 && len(b) > 0; i++ {
		c := b[len(b)-1-i]
		if c < 0x80 {
			return b
		}
		if c >= 0xC0 {
			// The start of a sequence: keep it only if it is complete.
			need := 2
			switch {
			case c >= 0xF0:
				need = 4
			case c >= 0xE0:
				need = 3
			}
			if i+1 >= need {
				return b
			}
			return b[:len(b)-1-i]
		}
	}
	return b
}

// DecodeRows reads a batch back: the request id and its rows. For tests and
// for the control plane's checks; the browser has its own decoder.
func DecodeRows(p []byte, cols int) (uint32, [][]*string, error) {
	if len(p) < 4 || cols <= 0 {
		return 0, nil, errors.New("tunnel: short row batch")
	}
	id := binary.BigEndian.Uint32(p)
	p = p[4:]
	var out [][]*string
	for len(p) > 0 {
		row := make([]*string, cols)
		for i := 0; i < cols; i++ {
			v, n := binary.Uvarint(p)
			if n <= 0 {
				return id, out, errors.New("tunnel: bad cell length")
			}
			p = p[n:]
			if v == 0 {
				continue
			}
			l := (v - 1) >> 1
			if l > uint64(len(p)) {
				return id, out, errors.New("tunnel: cell runs past the batch")
			}
			s := string(p[:l])
			row[i] = &s
			p = p[l:]
		}
		out = append(out, row)
	}
	return id, out, nil
}

// PGVerification is what checking a database recording's chain found.
type PGVerification struct {
	// OK is true when every line's hash is right and the last one is Head.
	OK bool
	// Lines is how many records were read.
	Lines int
	// Head is the last line's hash as computed, which an intact recording
	// shares with what the agent sealed.
	Head string
	// Broken is the sequence number of the first line whose hash or prev
	// link did not hold, or 0 when all held.
	Broken int64
}

// VerifyPGRecording recomputes a database recording's hash chain. A line is
// verified by hashing it with its hash field cleared and comparing; each
// line must also name the previous line's hash. expectedHead empty means
// compute only.
//
// Memory is one line at a time, whatever the recording's size.
func VerifyPGRecording(r io.Reader, expectedHead string) (PGVerification, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 2*MaxCell+64*1024)
	var v PGVerification
	prev := ""
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec PGRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			return v, fmt.Errorf("line %d: %w", v.Lines+1, err)
		}
		v.Lines++
		want := rec.Hash
		rec.Hash = ""
		body, err := json.Marshal(rec)
		if err != nil {
			return v, err
		}
		sum := sha256.Sum256(body)
		got := hex.EncodeToString(sum[:])
		if rec.Prev != prev || got != want {
			if v.Broken == 0 {
				v.Broken = rec.Seq
			}
		}
		prev = got
	}
	if err := sc.Err(); err != nil {
		return v, err
	}
	v.Head = prev
	v.OK = v.Broken == 0 && v.Lines > 0 && (expectedHead == "" || expectedHead == v.Head)
	return v, nil
}

// IsPGRecording reports whether a recording's first bytes look like a
// database recording rather than an asciicast: a JSON line with a seq field.
func IsPGRecording(head []byte) bool {
	head = bytes.TrimLeft(head, " \t\r\n")
	return bytes.HasPrefix(head, []byte(`{"seq":`))
}
