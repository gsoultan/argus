package auth

import (
	"encoding/base32"
	"strings"
	"testing"
	"time"
)

// The RFC 6238 seed, as base32 -- the ASCII string "12345678901234567890".
func rfcSecret() string {
	return base32.StdEncoding.WithPadding(base32.NoPadding).
		EncodeToString([]byte("12345678901234567890"))
}

// The published vectors from RFC 6238 Appendix B, SHA-1 column.
//
// Pinned here because an implementation that compiles and produces plausible
// six-digit numbers can still be wrong in a way no amount of manual testing
// with a phone would reveal -- until it locks everyone out, or worse, accepts
// codes it should not.
func TestTOTPMatchesRFC6238Vectors(t *testing.T) {
	secret := rfcSecret()
	for _, tc := range []struct {
		unix int64
		want string // 8 digits, as the RFC tabulates them
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	} {
		key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
		if err != nil {
			t.Fatal(err)
		}
		counter := uint64(tc.unix) / uint64(TOTPPeriod.Seconds())
		if got := hotp(key, counter, 8); got != tc.want {
			t.Errorf("T=%d: got %s, want %s", tc.unix, got, tc.want)
		}
	}
}

// Our six-digit codes are the last six digits of the RFC's eight-digit ones,
// which is what truncating to a smaller modulus means.
func TestTOTPSixDigitsAgreeWithTheVectors(t *testing.T) {
	secret := rfcSecret()
	for _, tc := range []struct {
		unix int64
		want string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1234567890, "005924"},
	} {
		got, err := TOTPCode(secret, time.Unix(tc.unix, 0))
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("T=%d: got %s, want %s", tc.unix, got, tc.want)
		}
	}
}

func TestVerifyTOTPAcceptsTheCurrentCode(t *testing.T) {
	secret, err := NewTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	code, err := TOTPCode(secret, now)
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyTOTP(secret, code, now) {
		t.Error("the current code must verify")
	}
	// A phone whose clock drifts, or a person typing as the code rolls over.
	if !VerifyTOTP(secret, code, now.Add(TOTPPeriod)) {
		t.Error("the previous step must still be accepted")
	}
	if !VerifyTOTP(secret, code, now.Add(-TOTPPeriod)) {
		t.Error("the next step must be accepted")
	}
}

// The window has to end somewhere, or a code lifted from a screenshot stays
// good indefinitely.
func TestVerifyTOTPRejectsCodesOutsideTheWindow(t *testing.T) {
	secret, _ := NewTOTPSecret()
	now := time.Now()
	code, _ := TOTPCode(secret, now)
	for _, skew := range []time.Duration{2 * TOTPPeriod, -2 * TOTPPeriod, time.Hour} {
		if VerifyTOTP(secret, code, now.Add(skew)) {
			t.Errorf("a code %v away must be refused", skew)
		}
	}
}

func TestVerifyTOTPRejectsMalformedInput(t *testing.T) {
	secret, _ := NewTOTPSecret()
	now := time.Now()
	for _, bad := range []string{"", "12345", "1234567", "abcdef", "  "} {
		if VerifyTOTP(secret, bad, now) {
			t.Errorf("%q must be refused", bad)
		}
	}
	// A code that is well-formed but simply wrong.
	if VerifyTOTP(secret, "000000", now) {
		code, _ := TOTPCode(secret, now)
		if code != "000000" {
			t.Error("an incorrect code must be refused")
		}
	}
	// And a secret that is not base32 at all.
	if VerifyTOTP("not base32 !!", "123456", now) {
		t.Error("an unreadable secret must not verify anything")
	}
}

func TestNewTOTPSecretIsUsableAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 20 {
		s, err := NewTOTPSecret()
		if err != nil {
			t.Fatal(err)
		}
		if seen[s] {
			t.Fatal("NewTOTPSecret returned a duplicate")
		}
		seen[s] = true
		// Apps reject padding, and reject secrets they cannot decode.
		if strings.Contains(s, "=") {
			t.Error("secret must not be padded")
		}
		if _, err := TOTPCode(s, time.Now()); err != nil {
			t.Errorf("fresh secret does not produce a code: %v", err)
		}
	}
}

// The URI is what an authenticator app scans; a malformed one fails silently
// at the worst possible moment, when someone is enrolling.
func TestTOTPEnrolmentURI(t *testing.T) {
	uri := TOTPEnrolmentURI("Argus", "lin@northwind.id", "ABCDEF")
	for _, want := range []string{
		"otpauth://totp/", "secret=ABCDEF", "issuer=Argus",
		"digits=6", "period=30", "algorithm=SHA1",
	} {
		if !strings.Contains(uri, want) {
			t.Errorf("URI missing %q: %s", want, uri)
		}
	}
	// The account has to be identifiable in the app's list.
	if !strings.Contains(uri, "lin%40northwind.id") && !strings.Contains(uri, "lin@northwind.id") {
		t.Errorf("URI does not carry the account: %s", uri)
	}
}
