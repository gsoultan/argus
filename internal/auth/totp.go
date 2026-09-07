package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Time-based one-time passwords, RFC 6238.
//
// Implemented here rather than pulled in, because the algorithm is forty lines
// and fully specified with published test vectors -- so it can be proven
// correct in this repository rather than trusted. For a product whose whole
// job is proving who did what, a dependency in the authentication path is
// worth avoiding when the alternative is this small.
//
// SHA-1 is what RFC 6238 specifies and what every authenticator app implements.
// It is used here as an HMAC, where the collision weaknesses that retired SHA-1
// for signatures do not apply.

const (
	// TOTPPeriod is the step every authenticator app assumes.
	TOTPPeriod = 30 * time.Second
	// TOTPDigits is the code length. Six, because that is what apps display.
	TOTPDigits = 6
	// TOTPSkew is how many steps either side of now are accepted.
	//
	// One step: enough for a phone clock that drifts a few seconds or a person
	// typing as the code rolls over, without widening the window a stolen code
	// can be replayed in. Zero would reject honest users constantly.
	TOTPSkew = 1
)

// NewTOTPSecret returns a fresh base32 secret of the length RFC 4226 recommends.
func NewTOTPSecret() (string, error) {
	b := make([]byte, 20) // 160 bits
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate TOTP secret: %w", err)
	}
	// No padding: authenticator apps and otpauth URIs do not expect it.
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b), nil
}

// TOTPCode computes the code for a secret at a moment.
func TOTPCode(secret string, at time.Time) (string, error) {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).
		DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return "", fmt.Errorf("decode TOTP secret: %w", err)
	}
	counter := uint64(at.Unix()) / uint64(TOTPPeriod.Seconds())
	return hotp(key, counter, TOTPDigits), nil
}

// hotp is RFC 4226: HMAC the counter, take a 4-byte window at a dynamic offset,
// and render it as decimal digits.
func hotp(key []byte, counter uint64, digits int) string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)

	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)

	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff

	mod := uint32(1)
	for range digits {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, value%mod)
}

// VerifyTOTP reports whether code is valid for secret around now.
//
// The comparison is constant time, and every candidate step is evaluated rather
// than returning on the first match, so the time taken does not reveal which
// step matched -- which would narrow the window for a replay.
func VerifyTOTP(secret, code string, now time.Time) bool {
	code = strings.TrimSpace(code)
	if len(code) != TOTPDigits {
		return false
	}
	ok := 0
	for skew := -TOTPSkew; skew <= TOTPSkew; skew++ {
		candidate, err := TOTPCode(secret, now.Add(time.Duration(skew)*TOTPPeriod))
		if err != nil {
			return false
		}
		ok |= subtle.ConstantTimeCompare([]byte(candidate), []byte(code))
	}
	return ok == 1
}

// TOTPEnrolmentURI is the otpauth:// URI an authenticator app scans.
//
// The issuer appears twice by convention: as a prefix on the label and as a
// parameter. Apps disagree about which they read, and one that shows an
// unlabelled six-digit code is one nobody can identify later.
func TOTPEnrolmentURI(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(TOTPDigits))
	q.Set("period", fmt.Sprint(int(TOTPPeriod.Seconds())))
	return "otpauth://totp/" + label + "?" + q.Encode()
}
