// Package credssp implements the Credential Security Support Provider protocol
// that RDP calls network level authentication.
//
// It is what lets Argus authenticate to a Windows host on a user's behalf. The
// SSH side already does the equivalent: the gateway injects a credential the
// user never sees, so nobody holds a standing key to a production machine.
// Without this, RDP stops one step short — Argus brokers and records the
// session, but the user still meets a logon screen and still needs a password
// of their own.
//
// CredSSP runs inside the TLS channel that has already been established:
//
//  1. SPNEGO/NTLM token exchange, carried in TSRequest structures
//  2. the server's TLS public key, returned encrypted, which is what binds the
//     authentication to this particular channel
//  3. the credentials themselves, encrypted under the negotiated key
//
// Step 2 is the reason CredSSP is not simply NTLM over TLS. It is also the step
// that makes a man in the middle detectable, which matters here because Argus
// deliberately is one.
package credssp

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf16"

	"golang.org/x/crypto/md4"
)

// NTLM message types (MS-NLMP 2.2).
const (
	MsgNegotiate    uint32 = 1
	MsgChallenge    uint32 = 2
	MsgAuthenticate uint32 = 3
)

var ntlmSignature = [8]byte{'N', 'T', 'L', 'M', 'S', 'S', 'P', 0}

// Negotiate flags (MS-NLMP 2.2.2.5). Only those Argus sets or checks.
const (
	NegotiateUnicode         uint32 = 0x00000001
	NegotiateSign            uint32 = 0x00000010
	NegotiateSeal            uint32 = 0x00000020
	NegotiateNTLM            uint32 = 0x00000200
	NegotiateAlwaysSign      uint32 = 0x00008000
	NegotiateExtendedSession uint32 = 0x00080000
	NegotiateTargetInfo      uint32 = 0x00800000
	NegotiateVersion         uint32 = 0x02000000
	Negotiate128             uint32 = 0x20000000
	NegotiateKeyExchange     uint32 = 0x40000000
	Negotiate56              uint32 = 0x80000000
)

// ErrMalformed reports a message that does not parse.
//
// These arrive from the target over the network, so every length and offset in
// them is untrusted. A malformed message must be an error rather than a partial
// read: authenticating against fields recovered from a broken structure is how
// a downgrade goes unnoticed.
var ErrMalformed = errors.New("malformed NTLM message")

// utf16le encodes a string the way NTLM requires.
func utf16le(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 0, len(units)*2)
	for _, u := range units {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

// NTOWFv2 derives the NTLMv2 one-way function of a password.
//
//	NTOWFv2(user, password, domain) =
//	    HMAC_MD5(MD4(UTF16LE(password)), UTF16LE(uppercase(user) + domain))
//
// MD4 and MD5 are both broken as hashes and are used here because the protocol
// specifies them; nothing about this construction is a choice Argus makes. It
// matters only that it matches what Windows computes.
func NTOWFv2(user, password, domain string) []byte {
	h := md4.New()
	h.Write(utf16le(password))
	ntlmHash := h.Sum(nil)

	// The user name is upper-cased; the domain is not. Upper-casing the domain
	// as well is the classic way to produce an implementation that authenticates
	// against some directories and not others.
	mac := hmac.New(md5.New, ntlmHash)
	mac.Write(utf16le(strings.ToUpper(user) + domain))
	return mac.Sum(nil)
}

// NTLMv2Response computes the NT response and the session base key.
//
// temp is the blob the response is computed over: a fixed header, the
// timestamp, the client challenge, and the server's target information.
func NTLMv2Response(responseKey, serverChallenge, clientChallenge []byte,
	timestamp uint64, targetInfo []byte) (response, sessionBaseKey []byte) {

	temp := make([]byte, 0, 28+len(targetInfo)+4)
	temp = append(temp, 0x01, 0x01, 0, 0) // Responserversion, HiResponserversion, Z(2)
	temp = append(temp, 0, 0, 0, 0)       // Z(4)
	temp = binary.LittleEndian.AppendUint64(temp, timestamp)
	temp = append(temp, clientChallenge...)
	temp = append(temp, 0, 0, 0, 0) // Z(4)
	temp = append(temp, targetInfo...)
	temp = append(temp, 0, 0, 0, 0) // Z(4)

	mac := hmac.New(md5.New, responseKey)
	mac.Write(serverChallenge)
	mac.Write(temp)
	ntProofStr := mac.Sum(nil)

	response = append(append([]byte{}, ntProofStr...), temp...)

	// The session base key is an HMAC over the proof string, not over the
	// response: including temp would make it depend on the target info, which
	// the server does not repeat back.
	mac = hmac.New(md5.New, responseKey)
	mac.Write(ntProofStr)
	sessionBaseKey = mac.Sum(nil)
	return response, sessionBaseKey
}

/* ── NEGOTIATE ───────────────────────────────────────────────────────────── */

// Negotiate builds the first NTLM message.
//
// Deliberately requests Unicode, extended session security, sealing and 128-bit
// keys. NTLMv1 and LM responses are never offered: they are trivially crackable
// and a server that would accept them will also accept v2, so there is no
// compatibility gained by leaving the door open.
func Negotiate() []byte {
	flags := NegotiateUnicode | NegotiateNTLM | NegotiateAlwaysSign |
		NegotiateExtendedSession | NegotiateSign | NegotiateSeal |
		NegotiateKeyExchange | Negotiate128 | Negotiate56 | NegotiateVersion

	msg := make([]byte, 0, 40)
	msg = append(msg, ntlmSignature[:]...)
	msg = binary.LittleEndian.AppendUint32(msg, MsgNegotiate)
	msg = binary.LittleEndian.AppendUint32(msg, flags)
	// Domain and workstation are empty: the target info in the challenge is
	// what actually scopes the authentication, and sending a name here only
	// tells an unauthenticated peer something about the gateway.
	msg = append(msg, 0, 0, 0, 0, 0, 0, 0, 0)  // domain: len, maxlen, offset
	msg = append(msg, 0, 0, 0, 0, 0, 0, 0, 0)  // workstation
	msg = append(msg, 6, 1, 0, 0, 0, 0, 0, 15) // version: 6.1 build 0, NTLMSSP rev 15
	return msg
}

/* ── CHALLENGE ───────────────────────────────────────────────────────────── */

// Challenge is the server's reply.
type Challenge struct {
	Flags uint32
	// ServerChallenge is the eight-byte nonce the response is computed over.
	ServerChallenge []byte
	// TargetName is the server's own name for itself.
	TargetName string
	// TargetInfo is an opaque AV_PAIR list echoed back in the response. It
	// carries the channel binding and the server's timestamp, so it must be
	// returned exactly as received rather than reconstructed.
	TargetInfo []byte
}

// ParseChallenge decodes a CHALLENGE_MESSAGE.
func ParseChallenge(b []byte) (Challenge, error) {
	var c Challenge
	if len(b) < 48 {
		return c, fmt.Errorf("%w: %d bytes, want at least 48", ErrMalformed, len(b))
	}
	if !bytes.Equal(b[:8], ntlmSignature[:]) {
		return c, fmt.Errorf("%w: bad signature", ErrMalformed)
	}
	if t := binary.LittleEndian.Uint32(b[8:12]); t != MsgChallenge {
		return c, fmt.Errorf("%w: message type %d, want %d", ErrMalformed, t, MsgChallenge)
	}

	targetName, err := readField(b, 12)
	if err != nil {
		return c, err
	}
	c.Flags = binary.LittleEndian.Uint32(b[20:24])
	c.ServerChallenge = append([]byte{}, b[24:32]...)

	info, err := readField(b, 40)
	if err != nil {
		return c, err
	}
	c.TargetInfo = info
	c.TargetName = decodeUTF16(targetName)
	return c, nil
}

// readField reads one length/offset-described payload.
//
// Every one of these is attacker-controlled: the offset and length come from
// the message being parsed. Bounds are checked against the actual buffer rather
// than trusted, and an out-of-range field is an error rather than a truncation,
// because silently returning fewer bytes would mean authenticating over
// something other than what the server sent.
func readField(b []byte, at int) ([]byte, error) {
	if at+8 > len(b) {
		return nil, fmt.Errorf("%w: field descriptor at %d is past the message", ErrMalformed, at)
	}
	length := int(binary.LittleEndian.Uint16(b[at : at+2]))
	offset := int(binary.LittleEndian.Uint32(b[at+4 : at+8]))
	if length == 0 {
		return nil, nil
	}
	if offset < 0 || length < 0 || offset > len(b) || offset+length > len(b) {
		return nil, fmt.Errorf("%w: field at %d claims %d bytes at offset %d, message is %d",
			ErrMalformed, at, length, offset, len(b))
	}
	return append([]byte{}, b[offset:offset+length]...), nil
}

func decodeUTF16(b []byte) string {
	if len(b)%2 != 0 {
		return ""
	}
	units := make([]uint16, 0, len(b)/2)
	for i := 0; i < len(b); i += 2 {
		units = append(units, binary.LittleEndian.Uint16(b[i:i+2]))
	}
	return string(utf16.Decode(units))
}

/* ── AUTHENTICATE ────────────────────────────────────────────────────────── */

// Credentials are what Argus presents on the user's behalf.
type Credentials struct {
	Domain      string
	User        string
	Password    string
	Workstation string
}

// BuildAuthenticate builds the third NTLM message and returns the exported
// session key, which every later CredSSP step encrypts under.
func BuildAuthenticate(c Challenge, creds Credentials, clientChallenge []byte,
	timestamp uint64) (msg, exportedSessionKey []byte, err error) {

	if len(clientChallenge) != 8 {
		return nil, nil, errors.New("client challenge must be eight bytes")
	}
	if len(c.ServerChallenge) != 8 {
		return nil, nil, fmt.Errorf("%w: server challenge is %d bytes",
			ErrMalformed, len(c.ServerChallenge))
	}

	responseKey := NTOWFv2(creds.User, creds.Password, creds.Domain)
	ntResponse, sessionBaseKey := NTLMv2Response(
		responseKey, c.ServerChallenge, clientChallenge, timestamp, c.TargetInfo)

	// With NTLMv2 the key exchange key is the session base key.
	keyExchangeKey := sessionBaseKey

	var encryptedRandomSessionKey []byte
	if c.Flags&NegotiateKeyExchange != 0 {
		exportedSessionKey = make([]byte, 16)
		if _, err := rand.Read(exportedSessionKey); err != nil {
			// A predictable session key would make every later message
			// forgeable, so this cannot fall back to anything.
			return nil, nil, fmt.Errorf("generate session key: %w", err)
		}
		encryptedRandomSessionKey = rc4Crypt(keyExchangeKey, exportedSessionKey)
	} else {
		exportedSessionKey = keyExchangeKey
	}

	// LM response is a fixed 24 zero bytes: NTLMv2 supersedes it, and sending a
	// real one would offer a downgrade path for free.
	lmResponse := make([]byte, 24)

	domain := utf16le(creds.Domain)
	user := utf16le(creds.User)
	workstation := utf16le(creds.Workstation)

	const headerLen = 64 + 8 // fixed header plus version
	offset := headerLen
	type field struct{ data []byte }
	fields := []field{{lmResponse}, {ntResponse}, {domain}, {user},
		{workstation}, {encryptedRandomSessionKey}}

	msg = make([]byte, 0, headerLen+len(ntResponse)+128)
	msg = append(msg, ntlmSignature[:]...)
	msg = binary.LittleEndian.AppendUint32(msg, MsgAuthenticate)

	// Descriptors are written in the order the message defines, each pointing
	// past the header into the payload section.
	for _, f := range fields {
		msg = binary.LittleEndian.AppendUint16(msg, uint16(len(f.data)))
		msg = binary.LittleEndian.AppendUint16(msg, uint16(len(f.data)))
		msg = binary.LittleEndian.AppendUint32(msg, uint32(offset))
		offset += len(f.data)
	}

	flags := c.Flags & (NegotiateUnicode | NegotiateNTLM | NegotiateAlwaysSign |
		NegotiateExtendedSession | NegotiateSign | NegotiateSeal |
		NegotiateKeyExchange | Negotiate128 | Negotiate56 | NegotiateTargetInfo)
	msg = binary.LittleEndian.AppendUint32(msg, flags)
	msg = append(msg, 6, 1, 0, 0, 0, 0, 0, 15) // version

	// The MIC is omitted, so the header is exactly headerLen and the offsets
	// computed above are correct.
	for _, f := range fields {
		msg = append(msg, f.data...)
	}
	return msg, exportedSessionKey, nil
}

// Timestamp renders a Go time as a Windows FILETIME.
func Timestamp(t time.Time) uint64 {
	const unixToFiletime = 116444736000000000 // 100ns intervals, 1601 to 1970
	return uint64(t.UnixNano()/100) + unixToFiletime
}
