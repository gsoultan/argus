package gateway

import (
	"regexp"
	"testing"
)

// Session ids are canonical UUIDs, because they are stored in one.
//
// They used to be bare hex. Postgres accepts 32 hex characters in a uuid column
// and prints them back dashed, so the same session had two spellings: the
// gateway named the recording object with one and the control plane stored the
// other. Nothing in the product compared them -- the console fetches a
// recording by the key it was handed -- but scripts/drill.sh has to map
// backwards, and for 12,268 recordings it could not find a chain head at all.
var canonical = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestSessionIdsAreCanonicalUUIDs(t *testing.T) {
	for _, tc := range []struct {
		name string
		gen  func() string
	}{
		{"ssh", newSessionID},
		{"rdp", newRDPSessionID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen := map[string]bool{}
			for i := 0; i < 500; i++ {
				id := tc.gen()
				if !canonical.MatchString(id) {
					t.Fatalf("%q is not a canonical UUID -- stored in a uuid "+
						"column it will read back as a different string than "+
						"the object key built from it", id)
				}
				if seen[id] {
					t.Fatalf("%q was generated twice in 500 draws", id)
				}
				seen[id] = true
			}
		})
	}
}

// The id and the object key built from it are the same string.
//
// This is the property that was missing, stated directly rather than implied by
// the format: whatever names the recording has to survive a round trip through
// the column the id is stored in.
func TestAnIdSurvivesBeingStoredAsAUUID(t *testing.T) {
	for i := 0; i < 100; i++ {
		id := newSessionID()
		// What Postgres does to a uuid on the way back out: lowercase
		// canonical form. An id already in that form is unchanged by it.
		if got := canonicalise(id); got != id {
			t.Fatalf("id %q reads back from a uuid column as %q; the object "+
				"key and the database row would disagree about the same session",
				id, got)
		}
	}
}

// canonicalise is what a uuid column does to a value: strip dashes, lowercase,
// and re-insert them in the canonical positions.
func canonicalise(id string) string {
	var hexOnly []byte
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
			hexOnly = append(hexOnly, c)
		case c >= 'A' && c <= 'F':
			hexOnly = append(hexOnly, c+('a'-'A'))
		}
	}
	if len(hexOnly) != 32 {
		return id
	}
	h := string(hexOnly)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
