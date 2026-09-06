package auth

import (
	"errors"
	"strings"
	"testing"
)

// A secret that ships in an example config is public knowledge. Length alone
// does not make one: signing session cookies and terminal tickets with a
// string an attacker can read off GitHub makes forging either arithmetic.
func TestNewSignerRefusesPlaceholders(t *testing.T) {
	for _, bad := range []string{
		"CHANGE-ME" + strings.Repeat("!", 30), // long enough, still a placeholder
		"argus-dev-signing-secret-at-least-32-chars-long",
		"  Change-Me  " + strings.Repeat(" ", 40),
	} {
		_, err := NewSigner(bad)
		if err == nil {
			t.Errorf("placeholder %q was accepted", bad)
			continue
		}
		// The message has to say how to generate a real one.
		if !strings.Contains(err.Error(), "openssl rand") {
			t.Errorf("%q: error should say how to fix it: %v", bad, err)
		}
	}
}

func TestNewSignerRefusesLowVariety(t *testing.T) {
	// 48 characters, one distinct rune: long, and worth exactly one character.
	if _, err := NewSigner(strings.Repeat("a", 48)); err == nil {
		t.Error("a secret of one repeated character was accepted")
	}
	if _, err := NewSigner(strings.Repeat("ab", 24)); err == nil {
		t.Error("a secret of two alternating characters was accepted")
	}
}

func TestNewSignerRefusesShortSecrets(t *testing.T) {
	if _, err := NewSigner("short"); err == nil {
		t.Error("a 5-character secret was accepted")
	}
}

// The values that actually ship must be refused by name, or the check is
// decorative. Padding a placeholder out to reach 32 characters is the common
// response to the length rule, and produces something no less public.
func TestTheShippedPlaceholdersAreRefused(t *testing.T) {
	for _, shipped := range []string{
		"CHANGE-ME-CHANGE-ME-CHANGE-ME-CHANGE-ME",
		"argus-dev-signing-secret-at-least-32-chars-long",
	} {
		if _, err := NewSigner(shipped); !errors.Is(err, ErrPlaceholderSecret) {
			t.Errorf("%q must be refused as a placeholder, got: %v", shipped, err)
		}
	}
}

// The sweeper used to run for the life of the process, holding a reference to
// the Signer so neither could ever be collected.
func TestSignerCloseStopsTheSweeper(t *testing.T) {
	s, err := NewSigner("Xk9mQ2vRt7YwEbN4hJcL8pAsDfGhJkLzXcVbNmQwErTy1234")
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s.Close()              // idempotent
	(*Signer)(nil).Close() // and safe on a nil signer
}

func TestNewSignerAcceptsARealSecret(t *testing.T) {
	// What `openssl rand -base64 48` produces.
	s, err := NewSigner("Xk9mQ2vRt7YwEbN4hJcL8pAsDfGhJkLzXcVbNmQwErTy1234")
	if err != nil {
		t.Fatalf("a random secret was refused: %v", err)
	}
	if s == nil {
		t.Fatal("nil signer with no error")
	}
}
