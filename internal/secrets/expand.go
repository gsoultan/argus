// Package secrets resolves credentials referenced from configuration files.
//
// Config files get committed, copied into tickets, pasted into chat, and
// included in bug reports. Anything genuinely secret therefore belongs
// somewhere else, with the file holding only a reference to it:
//
//	signing_secret: "${ARGUS_SIGNING_SECRET}"      # from the environment
//	reporter_token: "${file:/run/secrets/token}"   # from a mounted file
//	log_level:      "${LOG_LEVEL:-info}"           # with a default
//
// A reference that cannot be resolved is an error rather than an empty string.
// Silently expanding a missing secret to "" is how a service ends up running
// with an empty signing key and no indication anything is wrong.
package secrets

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// reference matches ${NAME}, ${NAME:-default} and ${file:/path}.
//
// Deliberately narrow: no nesting, no command substitution, no arithmetic. This
// resolves references, it is not a shell, and a config file is not a place to
// be executing anything.
var reference = regexp.MustCompile(`\$\{([A-Za-z0-9_:./\\-]+?)(?::-(.*?))?\}`)

// Missing lists references that could not be resolved.
type Missing struct {
	Names []string
}

func (m *Missing) Error() string {
	sort.Strings(m.Names)
	return fmt.Sprintf("unresolved configuration references: %s\n"+
		"Set them in the environment, or point them at a file with ${file:/path/to/secret}.",
		strings.Join(m.Names, ", "))
}

// Expand resolves every reference in data.
//
// All failures are collected before returning, so someone bringing up a new
// deployment sees every missing variable at once instead of rediscovering them
// one restart at a time.
func Expand(data []byte) ([]byte, error) {
	var missing []string
	seen := map[string]bool{}

	out := reference.ReplaceAllFunc(data, func(match []byte) []byte {
		groups := reference.FindSubmatch(match)
		name := string(groups[1])
		hasDefault := len(groups) > 2 && bytesNonNil(groups[2])
		fallback := string(groups[2])

		value, err := resolve(name)
		if err != nil {
			if hasDefault {
				return []byte(fallback)
			}
			if !seen[name] {
				seen[name] = true
				missing = append(missing, name)
			}
			return match
		}
		return []byte(value)
	})

	if len(missing) > 0 {
		return nil, &Missing{Names: missing}
	}
	return out, nil
}

// resolve looks up one reference.
func resolve(name string) (string, error) {
	if path, ok := strings.CutPrefix(name, "file:"); ok {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", path, err)
		}
		// Trailing newlines are near-universal in mounted secret files and are
		// never part of the value. A token with a stray \n fails
		// authentication in a way that is genuinely hard to see.
		return strings.TrimRight(string(data), "\r\n"), nil
	}

	value, ok := os.LookupEnv(name)
	if !ok {
		return "", fmt.Errorf("%s is not set", name)
	}
	// An explicitly empty variable is treated as unset. Nothing Argus reads
	// this way is meaningfully empty, and an empty signing key is far worse
	// than a startup failure.
	if value == "" {
		return "", fmt.Errorf("%s is set but empty", name)
	}
	return value, nil
}

func bytesNonNil(b []byte) bool { return b != nil }

// Redact replaces the middle of a secret so it can be logged for
// identification without disclosing it.
//
// Short values are replaced entirely: showing four of six characters is not
// redaction.
func Redact(s string) string {
	const keep = 4
	if len(s) < keep*3 {
		if s == "" {
			return ""
		}
		return "********"
	}
	return s[:keep] + strings.Repeat("*", 8) + s[len(s)-keep:]
}
