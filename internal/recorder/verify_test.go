package recorder

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

/*
Verify had no tests. It is the function the whole product's central claim rests
on -- that a recording is the one the gateway sealed -- and the only thing
exercising it was a shell drill nobody could run to completion.

The tamper case is a real artefact rather than one written for the occasion.
See testdata/README.md for what it is and how it was identified.
*/

// The chain refuses a recording relabelled to impersonate another session.
//
// This is the attack it exists to catch: take a real recording, rewrite the
// session it claims to be, and hope nobody recomputes the hash.
func TestARelabelledRecordingIsRefused(t *testing.T) {
	body, err := os.ReadFile("testdata/relabelled-session.cast")
	if err != nil {
		t.Fatal(err)
	}

	// Both sessions this file has ever been associated with: the one it claims
	// in its header, and the one its own trailer names.
	for _, tc := range []struct {
		name string
		head string
	}{
		{"the session it claims to be", "05a3592e749cde92abc824dbd6ce229b11910201141e3fb5b2a80eb42a1ebf10"},
		{"the session it was taken from", "4fbe4e8d97783a5e7e64e875d3ffd3367a4ed46b970da2324d39311ddba3e850"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Verify(bytes.NewReader(body), tc.head)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if v.OK {
				t.Fatal("a recording whose header and trailer name different " +
					"sessions verified as intact -- the chain accepted a swap")
			}
			if v.BrokenAt == 0 {
				t.Error("BrokenAt is 0 on a failed verification, so nothing " +
					"says where the recording stopped matching")
			}
		})
	}
}

// The fixture is the artefact described in testdata/README.md, not a file that
// drifted into looking like one.
func TestTheTamperFixtureIsStillTheOneDescribed(t *testing.T) {
	body, err := os.ReadFile("testdata/relabelled-session.cast")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, `"ARGUS_SESSION":"3ae54ffe7cd2faf2c920559e98b9f713"`) {
		t.Error("the header no longer claims session 3ae54ffe")
	}
	if !strings.Contains(text, "session f37acc687059e4f2750c542d1a1f44d5 recorded") {
		t.Error("the trailer no longer names session f37acc68")
	}
	if len(body) != 453 {
		t.Errorf("fixture is %d bytes, was 453 when it was identified", len(body))
	}
}

// The other direction: a recording the recorder actually sealed verifies
// against the head it published, or the test above proves nothing.
func TestARecordingVerifiesAgainstTheHeadItWasSealedWith(t *testing.T) {
	var buf bytes.Buffer
	rec, err := New(&buf, Header{Width: 80, Height: 24, Title: "ops@pay-01"})
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.Write(Output, []byte("uname -sm\r\n")); err != nil {
		t.Fatal(err)
	}
	if err := rec.Write(Output, []byte("Linux aarch64\n")); err != nil {
		t.Fatal(err)
	}
	head, err := rec.Close()
	if err != nil {
		t.Fatal(err)
	}

	v, err := Verify(bytes.NewReader(buf.Bytes()), head)
	if err != nil {
		t.Fatal(err)
	}
	if !v.OK {
		t.Fatalf("a recording did not verify against its own sealed head "+
			"(broke at line %d of %d)", v.BrokenAt, v.Lines)
	}
	if v.Head != head {
		t.Errorf("recomputed head %q, sealed head %q", v.Head, head)
	}

	// One byte changed anywhere in the body must break it.
	tampered := bytes.Replace(buf.Bytes(), []byte("Linux aarch64"), []byte("Linux aarch65"), 1)
	if len(tampered) != buf.Len() {
		t.Fatal("the tamper changed the length; it is meant to change only content")
	}
	if v, err := Verify(bytes.NewReader(tampered), head); err != nil {
		t.Fatal(err)
	} else if v.OK {
		t.Error("a single altered byte in the output still verified")
	}
}
