package tunnel

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRowEncoderRoundTrip(t *testing.T) {
	e := NewRowEncoder(7)
	long := bytes.Repeat([]byte("é"), MaxRowCell) // two bytes each, so the cut lands mid-rune
	e.Add([][]byte{[]byte("1"), nil, []byte("")})
	e.Add([][]byte{[]byte("ünïcødé"), []byte("x"), long})
	id, rows, err := DecodeRows(e.Batch(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if id != 7 || len(rows) != 2 {
		t.Fatalf("id %d rows %d", id, len(rows))
	}
	if *rows[0][0] != "1" || rows[0][1] != nil || *rows[0][2] != "" {
		t.Fatalf("row 0: NULL and empty must stay apart: %v", rows[0])
	}
	cut := *rows[1][2]
	if len(cut) > MaxRowCell || !utf8.ValidString(cut) {
		t.Fatalf("a cut value must stay within MaxRowCell and be valid text: %d bytes", len(cut))
	}
	e.Reset()
	if e.Rows() != 0 || len(e.Batch()) != 4 {
		t.Fatal("reset must keep only the id")
	}
}

func TestRowEncoderKeepsALongJSONWhole(t *testing.T) {
	// A 65,633-byte jsonb array of approval rules was cut at the old 64 KiB.
	doc := []byte(`[` + strings.Repeat(`{"name":"rule","then":["x"]},`, 2400) + `{}]`)
	e := NewRowEncoder(1)
	e.Add([][]byte{[]byte("1"), doc})
	_, rows, err := DecodeRows(e.Batch(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if *rows[0][1] != string(doc) || !json.Valid([]byte(*rows[0][1])) {
		t.Fatalf("a %d-byte value must arrive whole", len(doc))
	}
}

func TestRowEncoderKeepsEachBatchInOneFrame(t *testing.T) {
	big := bytes.Repeat([]byte("a"), MaxRowCell)
	row := [][]byte{[]byte("short"), big, big, big}
	e := NewRowEncoder(1)
	e.Add(row)
	if len(e.Batch()) > MaxFrame {
		t.Fatalf("one row of %d bytes made a %d-byte batch", 3*MaxRowCell, len(e.Batch()))
	}
	_, rows, err := DecodeRows(e.Batch(), 4)
	if err != nil {
		t.Fatal(err)
	}
	if *rows[0][0] != "short" || len(*rows[0][1]) != len(*rows[0][3]) || len(*rows[0][1]) >= MaxRowCell {
		t.Fatal("a row too big for a frame cuts its longest values evenly and keeps the short ones whole")
	}
	if e.Fits(row) {
		t.Fatal("a second such row must go in the next batch")
	}
	for e.Fits([][]byte{[]byte("x")}) {
		e.Add([][]byte{[]byte("x")})
		if len(e.Batch()) > MaxFrame {
			t.Fatal("batch past one frame")
		}
	}
}

func TestRowEncoderFlushes(t *testing.T) {
	e := NewRowEncoder(1)
	for i := 0; i < RowBatchRows; i++ {
		if e.Full() {
			t.Fatalf("full after %d rows", i)
		}
		e.Add([][]byte{[]byte("a")})
	}
	if !e.Full() {
		t.Fatal("a batch at RowBatchRows must be sent")
	}
}

func TestDecodeRowsRejectsOverrun(t *testing.T) {
	if _, _, err := DecodeRows([]byte{0, 0, 0, 1, 0x7f, 'a'}, 1); err == nil {
		t.Fatal("a cell claiming more bytes than the batch holds must be refused")
	}
}

func TestPersonalRole(t *testing.T) {
	for in, want := range map[string]string{
		"ana.sari@corp.example": "ana_sari",
		"Ana.Sari":              "ana_sari",
		"9lives":                "u9lives",
		"pg_admin":              "u_pg_admin",
		"argus_read":            "u_argus_read",
		"postgres":              "u_postgres",
		"":                      "",
		strings.Repeat("a", 80): strings.Repeat("a", 63),
	} {
		if got := PersonalRole(in); got != want {
			t.Errorf("PersonalRole(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLocalPGHost(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1": true, "::1": true, "localhost": true, "/var/run/postgresql": true,
		"10.0.0.5": false, "db.internal": false, "/var/run/../../etc": false, "": false,
		"relative/dir": false,
	} {
		if LocalPGHost(host) != want {
			t.Errorf("LocalPGHost(%q) != %v", host, want)
		}
	}
}

func TestGrantDBRoundTrips(t *testing.T) {
	key, _ := GrantKeyFromSecret(strings.Repeat("k", 32))
	g := NewGrant("a@b", "h", "ana", "")
	g.Service = PostgresService
	g.DB = &GrantDB{ID: "x", Name: "orders", Host: "127.0.0.1", Port: 5432, Level: LevelRead}
	tok, err := SignGrant(key, g)
	if err != nil {
		t.Fatal(err)
	}
	v := &GrantVerifier{Host: "h"}
	got, err := v.Verify(key.Public().(ed25519.PublicKey), tok)
	if err != nil {
		t.Fatal(err)
	}
	if got.DB == nil || got.DB.Name != "orders" || got.DB.Level != LevelRead || !got.ForService(PostgresService) {
		t.Fatalf("the database must survive signing: %+v", got.DB)
	}
	if got.ForService("terminal") {
		t.Fatal("a postgres grant must not open a terminal")
	}
}

func FuzzDecodeRows(f *testing.F) {
	e := NewRowEncoder(3)
	e.Add([][]byte{[]byte("a"), nil})
	f.Add(e.Batch(), 2)
	f.Fuzz(func(t *testing.T, p []byte, cols int) {
		if cols > 64 {
			cols = 64
		}
		_, _, _ = DecodeRows(p, cols)
	})
}

func BenchmarkRowEncoder(b *testing.B) {
	row := [][]byte{[]byte("48213"), []byte("2026-11-01 09:12:44+00"), []byte("rina.k@mail.id"),
		[]byte("1250000"), []byte("paid"), nil}
	e := NewRowEncoder(1)
	b.ReportAllocs()
	b.SetBytes(int64(len(row[0]) + len(row[1]) + len(row[2]) + len(row[3]) + len(row[4])))
	for b.Loop() {
		e.Add(row)
		if e.Full() {
			e.Reset()
		}
	}
}

// A recording's chain holds line by line; changing or dropping one is seen.
func TestVerifyPGRecording(t *testing.T) {
	var lines []string
	prev := ""
	for i := 1; i <= 4; i++ {
		r := PGRecord{Seq: int64(i), Time: "2026-01-01T00:00:00Z", Kind: "stmt", SQL: "select " + strconv.Itoa(i), Prev: prev}
		body, _ := json.Marshal(r)
		sum := sha256.Sum256(body)
		r.Hash = hex.EncodeToString(sum[:])
		line, _ := json.Marshal(r)
		lines = append(lines, string(line))
		prev = r.Hash
	}
	whole := strings.Join(lines, "\n") + "\n"
	v, err := VerifyPGRecording(strings.NewReader(whole), prev)
	if err != nil || !v.OK || v.Lines != 4 || v.Head != prev {
		t.Fatalf("intact: %+v %v", v, err)
	}
	if !IsPGRecording([]byte(whole)) {
		t.Fatal("must be recognised as a database recording")
	}
	// Wrong head.
	if v, _ := VerifyPGRecording(strings.NewReader(whole), "nope"); v.OK {
		t.Fatal("a wrong head must fail")
	}
	// A changed statement.
	tampered := strings.Replace(whole, "select 2", "select 9", 1)
	if v, _ := VerifyPGRecording(strings.NewReader(tampered), prev); v.OK || v.Broken != 2 {
		t.Fatalf("tampered: %+v", v)
	}
	// A dropped line.
	dropped := strings.Join(append(lines[:1:1], lines[2:]...), "\n")
	if v, _ := VerifyPGRecording(strings.NewReader(dropped), prev); v.OK || v.Broken != 3 {
		t.Fatalf("dropped: %+v", v)
	}
	// Empty.
	if v, _ := VerifyPGRecording(strings.NewReader(""), ""); v.OK {
		t.Fatal("an empty recording is not intact")
	}
}

// A line as long as an agent can send verifies. The scanner's buffer was
// 2*MaxCell+64KB, under MaxFrame, so a statement the agent recorded and sent
// whole read here as "token too long" and the replay was marked an error.
func TestVerifyPGRecordingTakesTheLongestLine(t *testing.T) {
	// A quote is two bytes once encoded: 120 KB of statement, 240 KB of line.
	sql := "SELECT '" + strings.Repeat(`"`, 120<<10) + "'"
	if !RecordedTextFits(sql) {
		t.Fatalf("a %d-byte statement should fit a line", len(sql))
	}
	r := PGRecord{Seq: 1, Time: "2026-01-01T00:00:00Z", Kind: "stmt", SQL: sql}
	body, _ := json.Marshal(r)
	sum := sha256.Sum256(body)
	r.Hash = hex.EncodeToString(sum[:])
	line, _ := json.Marshal(r)
	if len(line) <= 2*MaxCell+64<<10 || len(line) > MaxRecordLine {
		t.Fatalf("line of %d bytes does not exercise the old limit", len(line))
	}
	v, err := VerifyPGRecording(strings.NewReader(string(line)+"\n"), r.Hash)
	if err != nil || !v.OK {
		t.Fatalf("a long line: %+v %v", v, err)
	}
}

// What fits is decided on the encoded text, which can be six times longer
// than the statement: a statement short enough in bytes can still be too
// long to record.
func TestRecordedTextFits(t *testing.T) {
	if !RecordedTextFits("SELECT 1") {
		t.Fatal("a short statement must fit")
	}
	if RecordedTextFits(strings.Repeat("x", MaxSQL+1)) {
		t.Fatal("past MaxSQL must not fit")
	}
	escaped := strings.Repeat("<", MaxSQL/4) // <: six bytes each
	if len(escaped) > MaxSQL || RecordedTextFits(escaped) {
		t.Fatal("a statement that encodes past a frame must not fit")
	}
}

func TestPartAnswersFitAFrameAndRejoin(t *testing.T) {
	// Control characters escape to six bytes each in JSON; runes stay whole.
	value := []byte(strings.Repeat("\x01é\"", 200_000))
	kept := 1000
	var rest strings.Builder
	parts := PartAnswers(3, 5, 2, value, kept)
	for i, b := range parts {
		if len(b) > MaxFrame {
			t.Fatalf("part %d is %d bytes", i, len(b))
		}
		var a PGAnswer
		if err := json.Unmarshal(b, &a); err != nil || a.Type != "part" || a.Part.Row != 5 || a.Part.Col != 2 {
			t.Fatalf("part %d: %v %+v", i, err, a)
		}
		if a.Part.Last != (i == len(parts)-1) {
			t.Fatalf("only the last part says so (part %d)", i)
		}
		rest.WriteString(a.Part.Text)
	}
	if string(value[:kept])+rest.String() != string(value) {
		t.Fatal("the parts must rejoin to the value")
	}
}
