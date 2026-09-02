package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpandsFromEnvironment(t *testing.T) {
	t.Setenv("ARGUS_TEST_SECRET", "s3cr3t-value")

	out, err := Expand([]byte(`signing_secret: "${ARGUS_TEST_SECRET}"`))
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if got := string(out); got != `signing_secret: "s3cr3t-value"` {
		t.Errorf("got %q", got)
	}
}

func TestExpandsFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	// Mounted secret files almost always end in a newline, and a token with a
	// stray \n fails authentication in a way that is hard to see.
	if err := os.WriteFile(path, []byte("token-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := Expand([]byte(`reporter_token: "${file:` + path + `}"`))
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if !strings.Contains(string(out), `"token-from-file"`) {
		t.Errorf("got %q, want the trailing newline stripped", out)
	}
}

func TestDefaults(t *testing.T) {
	os.Unsetenv("ARGUS_TEST_ABSENT")
	out, err := Expand([]byte(`log_level: "${ARGUS_TEST_ABSENT:-info}"`))
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if got := string(out); got != `log_level: "info"` {
		t.Errorf("got %q, want the default applied", got)
	}
}

// The important one: a missing secret must stop startup, not become "".
func TestMissingReferenceIsAnError(t *testing.T) {
	os.Unsetenv("ARGUS_TEST_MISSING")

	out, err := Expand([]byte(`signing_secret: "${ARGUS_TEST_MISSING}"`))
	if err == nil {
		t.Fatalf("a missing secret expanded to %q instead of failing", out)
	}
	var missing *Missing
	if !errors.As(err, &missing) {
		t.Fatalf("error is %T, want *Missing", err)
	}
	if len(missing.Names) != 1 || missing.Names[0] != "ARGUS_TEST_MISSING" {
		t.Errorf("Names = %v", missing.Names)
	}
}

// An empty variable is as dangerous as a missing one — an empty signing key
// silently accepts forged tokens.
func TestEmptyVariableIsTreatedAsMissing(t *testing.T) {
	t.Setenv("ARGUS_TEST_EMPTY", "")
	if _, err := Expand([]byte(`signing_secret: "${ARGUS_TEST_EMPTY}"`)); err == nil {
		t.Error("an empty variable was accepted as a secret")
	}
}

// Someone bringing up a new deployment should see everything that is missing
// at once, not rediscover them one restart at a time.
func TestAllMissingReferencesAreReportedTogether(t *testing.T) {
	for _, n := range []string{"ARGUS_T_A", "ARGUS_T_B", "ARGUS_T_C"} {
		os.Unsetenv(n)
	}
	_, err := Expand([]byte(`
a: "${ARGUS_T_A}"
b: "${ARGUS_T_B}"
c: "${ARGUS_T_C}"
`))
	var missing *Missing
	if !errors.As(err, &missing) {
		t.Fatalf("error is %T, want *Missing", err)
	}
	if len(missing.Names) != 3 {
		t.Errorf("reported %d missing, want 3: %v", len(missing.Names), missing.Names)
	}
	// And the message should name them, so it is actionable without a debugger.
	for _, n := range []string{"ARGUS_T_A", "ARGUS_T_B", "ARGUS_T_C"} {
		if !strings.Contains(err.Error(), n) {
			t.Errorf("error message does not mention %s", n)
		}
	}
}

func TestRepeatedReferenceIsReportedOnce(t *testing.T) {
	os.Unsetenv("ARGUS_T_DUP")
	_, err := Expand([]byte(`a: "${ARGUS_T_DUP}"` + "\n" + `b: "${ARGUS_T_DUP}"`))
	var missing *Missing
	if !errors.As(err, &missing) {
		t.Fatalf("error is %T", err)
	}
	if len(missing.Names) != 1 {
		t.Errorf("Names = %v, want one entry", missing.Names)
	}
}

// This resolves references; it is not a shell. A config file must not be able
// to run anything.
func TestNoCommandSubstitution(t *testing.T) {
	for _, input := range []string{
		"a: $(whoami)",
		"b: `id`",
		"c: ${ARGUS_X:-$(rm -rf /)}",
	} {
		out, err := Expand([]byte(input))
		if err != nil {
			continue // an unresolvable reference is fine; execution is not
		}
		if strings.Contains(string(out), "root") || strings.Contains(string(out), "uid=") {
			t.Errorf("input %q appears to have executed something: %q", input, out)
		}
	}
}

func TestUnreferencedContentIsUntouched(t *testing.T) {
	in := "listen: \"0.0.0.0:8080\"\n# a price of $5 and a literal ${ not closed\n"
	out, err := Expand([]byte(in))
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if string(out) != in {
		t.Errorf("content changed:\n got %q\nwant %q", out, in)
	}
}

func TestRedact(t *testing.T) {
	cases := map[string]string{
		"":                                 "",
		"short":                            "********",
		"example-value-for-redaction-test": "exam********test",
	}
	for in, want := range cases {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
	// The whole point: the secret must not survive redaction. Deliberately not
	// a real dev value, so a repo-wide sweep for leaked secrets stays useful.
	secret := "example-not-a-real-credential-value-here"
	if strings.Contains(Redact(secret), "not-a-real-credential") {
		t.Error("Redact leaked the middle of the value")
	}
}
