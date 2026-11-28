package vault

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Limits on what a caller may send. Each bounds something stored or held in
// memory per request.
const (
	MaxNameLen     = 255
	maxSegments    = 8
	maxSegmentLen  = 63
	MaxValueBytes  = 64 << 10
	maxFields      = 64
	maxFieldLen    = 128
	maxTags        = 32
	maxTagLen      = 128
	maxDescription = 1024
	// maxVersions is how many versions are kept per secret. Older ones that
	// no label points at are dropped when a new one is added.
	maxVersions = 50
)

// Secret types.
const (
	TypeText = "text"
	TypeKV   = "kv"
	TypeFile = "file"
)

// Labels a version can carry.
const (
	LabelCurrent  = "current"
	LabelPrevious = "previous"
	LabelPending  = "pending"
)

// ValidName checks a secret name: lowercase path segments of letters, digits,
// '.', '_' and '-', separated by '/'. "prod/payments/db".
//
// Narrow on purpose: names appear in audit entries, URLs, file paths the agent
// writes under /run/argus/secrets, and environment variable references.
func ValidName(name string) error {
	if name == "" || len(name) > MaxNameLen {
		return fmt.Errorf("a secret name is 1 to %d characters", MaxNameLen)
	}
	segs := strings.Split(name, "/")
	if len(segs) > maxSegments {
		return fmt.Errorf("a secret name has at most %d parts", maxSegments)
	}
	for _, s := range segs {
		if err := validSegment(s); err != nil {
			return fmt.Errorf("%q: %w", name, err)
		}
	}
	return nil
}

// ValidPrefix checks a folder: a name, optionally ending in '/'. Empty means
// everything.
func ValidPrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	return ValidName(strings.TrimSuffix(prefix, "/"))
}

func validSegment(s string) error {
	if s == "" || len(s) > maxSegmentLen {
		return fmt.Errorf("each part is 1 to %d characters", maxSegmentLen)
	}
	if s == "." || s == ".." {
		return errors.New("a part cannot be . or ..")
	}
	for i, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || (i > 0 && (r == '.' || r == '_' || r == '-'))
		if !ok {
			return errors.New("use lowercase letters, digits, and . _ - after the first character")
		}
	}
	return nil
}

// Folder is the top-level folder of a name: "prod" for "prod/payments/db".
// One argus-secret server keeps each top-level folder.
func Folder(name string) string {
	f, _, _ := strings.Cut(name, "/")
	return f
}

// ValidType checks a secret type.
func ValidType(t string) error {
	switch t {
	case TypeText, TypeKV, TypeFile:
		return nil
	}
	return fmt.Errorf("type is one of %s, %s, %s", TypeText, TypeKV, TypeFile)
}

// checkValue checks a value against its type. A key-value secret is a JSON
// object of string fields.
func checkValue(typ string, value []byte) error {
	if len(value) == 0 {
		return errors.New("the value is empty")
	}
	if len(value) > MaxValueBytes {
		return fmt.Errorf("a value is at most %d KiB", MaxValueBytes>>10)
	}
	if typ != TypeKV {
		return nil
	}
	fields, err := parseFields(value)
	if err != nil {
		return err
	}
	if len(fields) == 0 {
		return errors.New("a key-value secret needs at least one field")
	}
	return nil
}

func parseFields(value []byte) (map[string]string, error) {
	var fields map[string]string
	if err := json.Unmarshal(value, &fields); err != nil {
		return nil, errors.New("a key-value secret is a JSON object of text fields")
	}
	if len(fields) > maxFields {
		return nil, fmt.Errorf("a key-value secret has at most %d fields", maxFields)
	}
	for k := range fields {
		if k == "" || len(k) > maxFieldLen {
			return nil, fmt.Errorf("a field name is 1 to %d characters", maxFieldLen)
		}
	}
	return fields, nil
}

// field picks one field out of a key-value value.
func field(typ string, value []byte, name string) ([]byte, error) {
	if name == "" {
		return value, nil
	}
	if typ != TypeKV {
		return nil, errors.New("only a key-value secret has fields")
	}
	fields, err := parseFields(value)
	if err != nil {
		return nil, err
	}
	v, ok := fields[name]
	if !ok {
		return nil, fmt.Errorf("no field %q", name)
	}
	return []byte(v), nil
}

// fieldNames lists a key-value value's field names, never their values.
func fieldNames(typ string, value []byte) []string {
	if typ != TypeKV {
		return nil
	}
	fields, err := parseFields(value)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(fields))
	for k := range fields {
		out = append(out, k)
	}
	return sortedStrings(out)
}
