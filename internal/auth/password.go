package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Password hashing for local accounts.
//
// Argon2id, because a PAM console's own credential is exactly the thing worth
// making expensive to crack offline: whoever takes the database gets one shot
// per guess per core-second rather than billions. bcrypt would do, but argon2id
// is memory-hard, which is what defeats the GPU rigs that make bcrypt tractable.
//
// Parameters follow the OWASP recommendation for argon2id: 19 MiB, two passes,
// one lane. They are stored in the hash string rather than assumed, so raising
// them later re-hashes on next sign-in instead of invalidating every password.

const (
	argonMemory  = 19 * 1024 // KiB
	argonTime    = 2
	argonThreads = 1
	argonKeyLen  = 32
	argonSaltLen = 16
)

// ErrPasswordMismatch is returned when a password does not verify. It is
// deliberately the same error whatever the reason, so a caller cannot turn this
// into an oracle for which accounts exist.
var ErrPasswordMismatch = errors.New("password does not match")

// MinPasswordLength is the floor for a local account.
//
// Length rather than composition rules: a 12-character passphrase beats an
// 8-character one with a digit and a symbol, and composition rules mostly
// produce Password1! on a sticky note.
const MinPasswordLength = 12

// HashPassword returns a PHC-format argon2id string.
func HashPassword(password string) (string, error) {
	if len([]rune(password)) < MinPasswordLength {
		return "", fmt.Errorf("password must be at least %d characters", MinPasswordLength)
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	sum := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(sum),
	), nil
}

// VerifyPassword checks a password against a stored hash.
//
// Parameters come from the encoded hash, so a password stored under older
// settings still verifies after they are raised.
func VerifyPassword(encoded, password string) error {
	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=...,t=...,p=...", salt, hash]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return fmt.Errorf("%w: unrecognised hash format", ErrPasswordMismatch)
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return fmt.Errorf("%w: unsupported argon2 version", ErrPasswordMismatch)
	}
	var memory uint32
	var times uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &times, &threads); err != nil {
		return fmt.Errorf("%w: unreadable parameters", ErrPasswordMismatch)
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return fmt.Errorf("%w: unreadable salt", ErrPasswordMismatch)
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return fmt.Errorf("%w: unreadable digest", ErrPasswordMismatch)
	}

	got := argon2.IDKey([]byte(password), salt, times, memory, threads, uint32(len(want)))
	// Constant time: a timing difference here leaks how much of a guess was
	// correct, which is enough to recover a password byte by byte.
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrPasswordMismatch
	}
	return nil
}
