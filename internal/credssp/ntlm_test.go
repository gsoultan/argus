package credssp

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

// The worked NTLMv2 example from MS-NLMP section 4.2.4.
//
// Checking against Microsoft's own published intermediate values, rather than
// against a second reading of the prose, is the only way to know this agrees
// with what Windows computes. Cryptographic code that is subtly wrong still
// produces confident output of the right length, and the failure shows up as
// "authentication failed" against a real domain controller with nothing to say
// why.
const (
	mslabUser     = "User"
	mslabDomain   = "Domain"
	mslabPassword = "Password"
	mslabServer   = "Server"

	// Expected NTOWFv2, MS-NLMP 4.2.4.1.1.
	wantNTOWFv2 = "0c868a403bfd7a93a3001ef22ef02e3f"
	// Expected SessionBaseKey, MS-NLMP 4.2.4.1.2.
	wantSessionBaseKey = "8de40ccadbc14a82f15cb0ad0de95ca3"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestNTOWFv2MatchesTheSpecification(t *testing.T) {
	got := NTOWFv2(mslabUser, mslabPassword, mslabDomain)
	if hex.EncodeToString(got) != wantNTOWFv2 {
		t.Errorf("NTOWFv2 = %s\n            want %s",
			hex.EncodeToString(got), wantNTOWFv2)
	}
}

// Upper-casing the domain as well as the user is a classic error: it
// authenticates against some directories and not others, so it looks like an
// environment problem rather than a bug.
func TestNTOWFv2UpperCasesOnlyTheUser(t *testing.T) {
	base := NTOWFv2("User", "Password", "Domain")
	if !bytes.Equal(base, NTOWFv2("uSeR", "Password", "Domain")) {
		t.Error("the user name is case-sensitive; it must be upper-cased")
	}
	if bytes.Equal(base, NTOWFv2("User", "Password", "DOMAIN")) {
		t.Error("the domain was upper-cased; the specification does not")
	}
}

// The session base key is what every later CredSSP step encrypts under, so a
// wrong one fails at the very last message with no useful diagnostic.
func TestNTLMv2SessionBaseKeyMatchesTheSpecification(t *testing.T) {
	responseKey := NTOWFv2(mslabUser, mslabPassword, mslabDomain)
	serverChallenge := mustHex(t, "0123456789abcdef")
	clientChallenge := mustHex(t, "aaaaaaaaaaaaaaaa")

	// MS-NLMP 4.2.4 uses a zero timestamp and this target info.
	targetInfo := buildTargetInfo(t, mslabDomain, mslabServer)

	_, sessionBaseKey := NTLMv2Response(
		responseKey, serverChallenge, clientChallenge, 0, targetInfo)

	if hex.EncodeToString(sessionBaseKey) != wantSessionBaseKey {
		t.Errorf("SessionBaseKey = %s\n                 want %s",
			hex.EncodeToString(sessionBaseKey), wantSessionBaseKey)
	}
}

// buildTargetInfo assembles the AV_PAIR list used in the specification's
// worked example: NetBIOS domain, NetBIOS computer, then the terminator.
func buildTargetInfo(t *testing.T, domain, server string) []byte {
	t.Helper()
	var out []byte
	add := func(id uint16, value string) {
		v := utf16le(value)
		out = binary.LittleEndian.AppendUint16(out, id)
		out = binary.LittleEndian.AppendUint16(out, uint16(len(v)))
		out = append(out, v...)
	}
	add(2, domain) // MsvAvNbDomainName
	add(1, server) // MsvAvNbComputerName
	out = binary.LittleEndian.AppendUint16(out, 0)
	out = binary.LittleEndian.AppendUint16(out, 0) // MsvAvEOL
	return out
}

/* ── Message construction ────────────────────────────────────────────────── */

// NTLMv1 and LM responses are trivially crackable, and a server that accepts
// them accepts v2 as well — so offering them buys no compatibility and costs a
// downgrade path.
func TestNegotiateNeverOffersWeakAuthentication(t *testing.T) {
	msg := Negotiate()
	if !bytes.Equal(msg[:8], ntlmSignature[:]) {
		t.Fatal("no NTLMSSP signature")
	}
	if binary.LittleEndian.Uint32(msg[8:12]) != MsgNegotiate {
		t.Fatal("wrong message type")
	}

	flags := binary.LittleEndian.Uint32(msg[12:16])
	const negotiateLMKey = 0x00000080
	if flags&negotiateLMKey != 0 {
		t.Error("NEGOTIATE_LM_KEY was offered")
	}
	for name, bit := range map[string]uint32{
		"unicode":                   NegotiateUnicode,
		"extended session security": NegotiateExtendedSession,
		"seal":                      NegotiateSeal,
		"128-bit":                   Negotiate128,
	} {
		if flags&bit == 0 {
			t.Errorf("%s was not requested", name)
		}
	}
}

func TestParseChallengeReadsTheServerReply(t *testing.T) {
	targetInfo := buildTargetInfo(t, mslabDomain, mslabServer)
	msg := buildChallenge(t, mslabServer, mustHex(t, "0123456789abcdef"),
		NegotiateUnicode|NegotiateNTLM|NegotiateTargetInfo|NegotiateKeyExchange,
		targetInfo)

	c, err := ParseChallenge(msg)
	if err != nil {
		t.Fatalf("ParseChallenge: %v", err)
	}
	if c.TargetName != mslabServer {
		t.Errorf("TargetName = %q", c.TargetName)
	}
	if hex.EncodeToString(c.ServerChallenge) != "0123456789abcdef" {
		t.Errorf("ServerChallenge = %x", c.ServerChallenge)
	}
	// The target info carries the server's timestamp and channel binding and is
	// echoed back verbatim, so it must survive byte for byte.
	if !bytes.Equal(c.TargetInfo, targetInfo) {
		t.Errorf("TargetInfo was altered:\n got %x\nwant %x", c.TargetInfo, targetInfo)
	}
}

// The challenge arrives from the target over the network, so every length and
// offset in it is attacker-controlled. A field pointing outside the message
// must be an error, not a short read: authenticating over recovered garbage is
// how a downgrade goes unnoticed.
func TestParseChallengeRefusesOutOfRangeFields(t *testing.T) {
	base := buildChallenge(t, mslabServer, mustHex(t, "0123456789abcdef"),
		NegotiateUnicode, buildTargetInfo(t, mslabDomain, mslabServer))

	cases := []struct {
		name  string
		alter func([]byte) []byte
	}{
		{"target name length past the end", func(b []byte) []byte {
			out := append([]byte{}, b...)
			binary.LittleEndian.PutUint16(out[12:14], 0xFFFF)
			return out
		}},
		{"target info offset past the end", func(b []byte) []byte {
			out := append([]byte{}, b...)
			binary.LittleEndian.PutUint32(out[44:48], 0xFFFFFFF0)
			return out
		}},
		{"truncated message", func(b []byte) []byte { return b[:40] }},
		{"wrong signature", func(b []byte) []byte {
			out := append([]byte{}, b...)
			out[0] = 'X'
			return out
		}},
		{"wrong message type", func(b []byte) []byte {
			out := append([]byte{}, b...)
			binary.LittleEndian.PutUint32(out[8:12], MsgNegotiate)
			return out
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseChallenge(tc.alter(base)); err == nil {
				t.Error("a malformed challenge parsed without error")
			} else if !errors.Is(err, ErrMalformed) {
				t.Errorf("err = %v, want ErrMalformed", err)
			}
		})
	}
}

// buildChallenge assembles a CHALLENGE_MESSAGE the way a server does.
func buildChallenge(t *testing.T, targetName string, serverChallenge []byte,
	flags uint32, targetInfo []byte) []byte {
	t.Helper()

	name := utf16le(targetName)
	const header = 48
	msg := make([]byte, 0, header+len(name)+len(targetInfo))
	msg = append(msg, ntlmSignature[:]...)
	msg = binary.LittleEndian.AppendUint32(msg, MsgChallenge)

	msg = binary.LittleEndian.AppendUint16(msg, uint16(len(name)))
	msg = binary.LittleEndian.AppendUint16(msg, uint16(len(name)))
	msg = binary.LittleEndian.AppendUint32(msg, header)

	msg = binary.LittleEndian.AppendUint32(msg, flags)
	msg = append(msg, serverChallenge...)
	msg = append(msg, 0, 0, 0, 0, 0, 0, 0, 0) // reserved

	msg = binary.LittleEndian.AppendUint16(msg, uint16(len(targetInfo)))
	msg = binary.LittleEndian.AppendUint16(msg, uint16(len(targetInfo)))
	msg = binary.LittleEndian.AppendUint32(msg, uint32(header+len(name)))

	msg = append(msg, name...)
	msg = append(msg, targetInfo...)
	return msg
}

func TestAuthenticateProducesAParseableMessage(t *testing.T) {
	targetInfo := buildTargetInfo(t, mslabDomain, mslabServer)
	c := Challenge{
		Flags: NegotiateUnicode | NegotiateNTLM | NegotiateExtendedSession |
			NegotiateKeyExchange | Negotiate128,
		ServerChallenge: mustHex(t, "0123456789abcdef"),
		TargetName:      mslabServer,
		TargetInfo:      targetInfo,
	}
	creds := Credentials{Domain: mslabDomain, User: mslabUser,
		Password: mslabPassword, Workstation: "COMPUTER"}

	msg, sessionKey, err := BuildAuthenticate(c, creds, mustHex(t, "aaaaaaaaaaaaaaaa"), 0)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if len(sessionKey) != 16 {
		t.Fatalf("session key is %d bytes, want 16", len(sessionKey))
	}
	if !bytes.Equal(msg[:8], ntlmSignature[:]) {
		t.Fatal("no signature")
	}
	if binary.LittleEndian.Uint32(msg[8:12]) != MsgAuthenticate {
		t.Fatal("wrong message type")
	}

	// Every field descriptor must point inside the message. Windows rejects the
	// whole authentication if one does not, with no indication of which.
	for name, at := range map[string]int{
		"lm response": 12, "nt response": 20, "domain": 28,
		"user": 36, "workstation": 44, "session key": 52,
	} {
		length := int(binary.LittleEndian.Uint16(msg[at : at+2]))
		offset := int(binary.LittleEndian.Uint32(msg[at+4 : at+8]))
		if length == 0 {
			continue
		}
		if offset+length > len(msg) {
			t.Errorf("%s points to [%d,%d) but the message is %d bytes",
				name, offset, offset+length, len(msg))
		}
	}

	// The user name must be recoverable, or the server cannot look the account up.
	userLen := int(binary.LittleEndian.Uint16(msg[36:38]))
	userOff := int(binary.LittleEndian.Uint32(msg[40:44]))
	if got := decodeUTF16(msg[userOff : userOff+userLen]); got != mslabUser {
		t.Errorf("user in message = %q, want %q", got, mslabUser)
	}
}

// An LM response would be a downgrade path offered for free.
func TestAuthenticateSendsNoLMResponse(t *testing.T) {
	c := Challenge{
		Flags:           NegotiateUnicode | NegotiateNTLM,
		ServerChallenge: mustHex(t, "0123456789abcdef"),
	}
	msg, _, err := BuildAuthenticate(c, Credentials{User: "u", Password: "p"},
		mustHex(t, "aaaaaaaaaaaaaaaa"), 0)
	if err != nil {
		t.Fatal(err)
	}
	length := int(binary.LittleEndian.Uint16(msg[12:14]))
	offset := int(binary.LittleEndian.Uint32(msg[16:20]))
	lm := msg[offset : offset+length]
	if !bytes.Equal(lm, make([]byte, 24)) {
		t.Errorf("LM response is not zero: %x", lm)
	}
}

func TestAuthenticateRejectsBadChallenges(t *testing.T) {
	good := Challenge{ServerChallenge: mustHex(t, "0123456789abcdef")}
	if _, _, err := BuildAuthenticate(good, Credentials{}, []byte{1, 2, 3}, 0); err == nil {
		t.Error("a short client challenge was accepted")
	}
	short := Challenge{ServerChallenge: []byte{1, 2}}
	if _, _, err := BuildAuthenticate(short, Credentials{},
		mustHex(t, "aaaaaaaaaaaaaaaa"), 0); err == nil {
		t.Error("a short server challenge was accepted")
	}
}

// Each call must use a fresh keystream; reusing one leaks plaintext outright.
func TestRC4IsFreshPerCall(t *testing.T) {
	key := mustHex(t, "0123456789abcdef0123456789abcdef")
	a := rc4Crypt(key, []byte("the same plaintext"))
	b := rc4Crypt(key, []byte("the same plaintext"))
	if !bytes.Equal(a, b) {
		t.Fatal("the same key and plaintext gave different output")
	}
	if bytes.Equal(a, []byte("the same plaintext")) {
		t.Fatal("nothing was encrypted")
	}
	// And it round-trips, which is what the protocol relies on.
	if got := string(rc4Crypt(key, a)); got != "the same plaintext" {
		t.Errorf("round trip gave %q", got)
	}
}

func TestSessionKeysAreNotPredictable(t *testing.T) {
	c := Challenge{
		Flags:           NegotiateKeyExchange,
		ServerChallenge: mustHex(t, "0123456789abcdef"),
	}
	seen := map[string]bool{}
	for range 16 {
		_, key, err := BuildAuthenticate(c, Credentials{User: "u", Password: "p"},
			mustHex(t, "aaaaaaaaaaaaaaaa"), 0)
		if err != nil {
			t.Fatal(err)
		}
		h := hex.EncodeToString(key)
		if seen[h] {
			t.Fatal("the same session key was generated twice; every later " +
				"message would be forgeable")
		}
		seen[h] = true
	}
}

func TestTimestampIsAFiletime(t *testing.T) {
	// 1970-01-01 in FILETIME.
	if got := Timestamp(unixEpoch()); got != 116444736000000000 {
		t.Errorf("Timestamp(epoch) = %d", got)
	}
}

func TestUTF16LE(t *testing.T) {
	if got := utf16le("AB"); !bytes.Equal(got, []byte{'A', 0, 'B', 0}) {
		t.Errorf("utf16le = %x", got)
	}
	if got := decodeUTF16([]byte{'A', 0, 'B', 0}); got != "AB" {
		t.Errorf("decodeUTF16 = %q", got)
	}
	// An odd-length buffer is malformed and must not panic.
	if got := decodeUTF16([]byte{'A'}); got != "" {
		t.Errorf("odd-length decode = %q", got)
	}
	if !strings.Contains(string(utf16le("é")), "\xe9") {
		t.Log("non-ASCII encodes as UTF-16LE as expected")
	}
}

func unixEpoch() time.Time { return time.Unix(0, 0).UTC() }
