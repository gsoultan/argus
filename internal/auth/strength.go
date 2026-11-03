package auth

import (
	"crypto/rand"
	"errors"
	"math/big"
	"strings"
	"unicode"
)

// Password strength, for a password a person chooses.
//
// Length stays the main rule (MinPasswordLength), because length is what makes
// a password expensive to guess. On top of it, the handful of checks that stop
// the passwords people actually pick when told "at least twelve characters":
// Password1234, their own name or address, aaaaaaaaaaaa, qwertyuiop12. A long
// passphrase is accepted with no composition rule at all; a shorter password
// must mix kinds of character. Nothing here is a substitute for the argon2
// cost or the sign-in limits -- it only keeps the first guesses on any list
// from being right.

// PassphraseLength is the length at which no mix of characters is asked for:
// four or five ordinary words.
const PassphraseLength = 20

// ErrWeakPassword is wrapped by every refusal from CheckPasswordStrength.
var ErrWeakPassword = errors.New("password is too weak")

// WeakPasswordError says what is wrong with a password, in words a person can
// act on.
type WeakPasswordError struct{ Reason string }

func (e *WeakPasswordError) Error() string { return e.Reason }

// Unwrap lets callers test errors.Is(err, ErrWeakPassword).
func (e *WeakPasswordError) Unwrap() error { return ErrWeakPassword }

// PasswordContext is what a password must not be built from.
type PasswordContext struct {
	Email string
	Name  string
}

// CheckPasswordStrength returns nil for a password good enough to be someone's
// own, or a *WeakPasswordError saying why not.
func CheckPasswordStrength(pw string, c PasswordContext) error {
	weak := func(reason string) error { return &WeakPasswordError{Reason: reason} }
	runes := []rune(pw)
	if len(runes) < MinPasswordLength {
		return weak("use at least 12 characters")
	}
	if len(runes) > 256 {
		return weak("use at most 256 characters")
	}
	if strings.TrimSpace(pw) != pw {
		return weak("don't start or end the password with a space")
	}

	lower := strings.ToLower(pw)
	for _, part := range personalParts(c) {
		if strings.Contains(lower, part) {
			return weak("don't use your name or email address in the password")
		}
	}

	// Undo the usual substitutions and strip what is left over: "P@ssw0rd!"
	// and "Password2024" are both "password", which is on every list.
	plain := strings.Map(func(r rune) rune {
		switch r {
		case '@', '4':
			return 'a'
		case '0':
			return 'o'
		case '1', '!':
			return 'l'
		case '3':
			return 'e'
		case '$', '5':
			return 's'
		case '7':
			return 't'
		}
		if unicode.IsLetter(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, pw)
	for _, w := range commonPasswords {
		// A long stem anywhere in a short password; a short stem only as the
		// whole of it, or "admin" would refuse every password that mentions an
		// admiral. A passphrase may contain a word that is also on the list.
		if (len(w) >= 6 && len(runes) < PassphraseLength &&
			(strings.Contains(plain, w) || strings.Contains(lower, w))) ||
			plain == w || strings.TrimRight(lower, "0123456789!@#$%^&*.?") == w {
			return weak("that password is one of the first an attacker tries; choose another")
		}
	}

	distinct := map[rune]bool{}
	for _, r := range runes {
		distinct[r] = true
	}
	if len(distinct) < 6 {
		return weak("use more different characters")
	}
	if sequential(lower) {
		return weak("avoid runs like abcd, 1234 or qwerty")
	}

	if len(runes) >= PassphraseLength {
		return nil
	}
	var classes int
	var hasLower, hasUpper, hasDigit, hasOther bool
	for _, r := range runes {
		switch {
		case unicode.IsLower(r):
			hasLower = true
		case unicode.IsUpper(r):
			hasUpper = true
		case unicode.IsDigit(r):
			hasDigit = true
		default:
			hasOther = true
		}
	}
	for _, b := range []bool{hasLower, hasUpper, hasDigit, hasOther} {
		if b {
			classes++
		}
	}
	if classes < 3 {
		return weak("mix at least three of: lower case, upper case, digits and symbols, " +
			"or use a passphrase of 20 characters or more")
	}
	return nil
}

// personalParts are the pieces of someone's address and name long enough that
// finding one in their password is not a coincidence.
func personalParts(c PasswordContext) []string {
	var out []string
	add := func(s string) {
		s = strings.ToLower(strings.TrimSpace(s))
		if len([]rune(s)) >= 4 {
			out = append(out, s)
		}
	}
	local, domain, _ := strings.Cut(c.Email, "@")
	add(local)
	for _, p := range strings.FieldsFunc(local, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		add(p)
	}
	if host, _, ok := strings.Cut(domain, "."); ok {
		add(host)
	}
	for _, p := range strings.Fields(c.Name) {
		add(p)
	}
	return out
}

// sequential reports a run of five or more characters in keyboard or
// alphabet order (either direction), or a run of five of the same character.
func sequential(s string) bool {
	const rows = "abcdefghijklmnopqrstuvwxyz 0123456789 qwertyuiop asdfghjkl zxcvbnm"
	rev := func(x string) string {
		r := []rune(x)
		for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
			r[i], r[j] = r[j], r[i]
		}
		return string(r)
	}
	rs := []rune(s)
	for i := 0; i+5 <= len(rs); i++ {
		w := string(rs[i : i+5])
		if strings.Contains(rows, w) || strings.Contains(rows, rev(w)) {
			return true
		}
		if strings.Count(w, string(rs[i])) == 5 {
			return true
		}
	}
	return false
}

// commonPasswords are the stems that top every breach list, compared after
// lower-casing and with trailing digits and symbols taken off.
var commonPasswords = []string{
	"password", "letmein", "welcome", "admin",
	"administrator", "changeme", "iloveyou", "monkey", "dragon", "football",
	"baseball", "sunshine", "princess", "trustno", "superman", "batman",
	"master", "shadow", "michael", "qwerty", "starwars", "whatever",
	"argus", "secret", "default", "login", "access", "root", "toor",
	"passwordpassword", "correcthorsebatterystaple",
}

// temporaryAlphabet leaves out characters that are confused when read off a
// screen or an email: 0/O, 1/l/I.
const (
	tempLower   = "abcdefghijkmnpqrstuvwxyz"
	tempUpper   = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	tempDigits  = "23456789"
	tempSymbols = "-_.+=!@#%"
)

// TemporaryPasswordLength is long enough that the temporary password passes
// every rule above, and is ~100 bits from the alphabet.
const TemporaryPasswordLength = 18

// NewTemporaryPassword returns a random password for a person to use once,
// from an alphabet without look-alike characters so it can be read and typed.
func NewTemporaryPassword() (string, error) {
	all := tempLower + tempUpper + tempDigits + tempSymbols
	for {
		var b strings.Builder
		for i := 0; i < TemporaryPasswordLength; i++ {
			n, err := rand.Int(rand.Reader, big.NewInt(int64(len(all))))
			if err != nil {
				return "", err
			}
			b.WriteByte(all[n.Int64()])
		}
		pw := b.String()
		// Drawn at random, it nearly always qualifies; when it does not
		// (one class missing, a run), draw again rather than patch it.
		if CheckPasswordStrength(pw, PasswordContext{}) == nil &&
			strings.ContainsAny(pw, tempLower) && strings.ContainsAny(pw, tempUpper) &&
			strings.ContainsAny(pw, tempDigits) {
			return pw, nil
		}
	}
}
