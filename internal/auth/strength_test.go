package auth

import (
	"errors"
	"testing"
)

func TestCheckPasswordStrength(t *testing.T) {
	ctx := PasswordContext{Email: "dana.lee@example.com", Name: "Dana Lee"}
	for _, tc := range []struct {
		pw   string
		ok   bool
		what string
	}{
		{"short1A!", false, "under 12"},
		{"Password2024!", false, "a common stem with digits"},
		{"P@ssw0rd!!!!xY", false, "a common stem in leetspeak"},
		{"Dana.Lee#2026x", false, "their own name"},
		{"Example!2026xyZ", false, "their email's domain"},
		{"aaaaaaaaaaaaaa", false, "one character"},
		{"Abcdefg!23456", false, "an alphabet run"},
		{"Qwerty!9Lm#zz", false, "a keyboard run"},
		{"lowercaseonlyhere", false, "one class under 20"},
		{"horse cable violet river", true, "a passphrase"},
		{"Tr4in-Coffee-Nap", true, "three classes"},
		{" Tr4in-Coffee-Nap", false, "a leading space"},
	} {
		err := CheckPasswordStrength(tc.pw, ctx)
		if (err == nil) != tc.ok {
			t.Errorf("%s (%q): got %v, want ok=%v", tc.what, tc.pw, err, tc.ok)
		}
		if err != nil && !errors.Is(err, ErrWeakPassword) {
			t.Errorf("%s: %v is not ErrWeakPassword", tc.what, err)
		}
	}
}

func TestTemporaryPasswordsAreStrongAndDistinct(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		pw, err := NewTemporaryPassword()
		if err != nil {
			t.Fatal(err)
		}
		if len(pw) != TemporaryPasswordLength {
			t.Fatalf("%q is %d long", pw, len(pw))
		}
		if err := CheckPasswordStrength(pw, PasswordContext{}); err != nil {
			t.Fatalf("%q: %v", pw, err)
		}
		if seen[pw] {
			t.Fatalf("%q came up twice", pw)
		}
		seen[pw] = true
	}
}

func TestAPassphraseMayContainAListedWord(t *testing.T) {
	if err := CheckPasswordStrength("the dragon sleeps under seven hills", PasswordContext{}); err != nil {
		t.Fatal(err)
	}
}
