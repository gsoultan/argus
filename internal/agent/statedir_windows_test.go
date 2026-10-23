package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/gsoultan/argus/internal/secrets"
)

// grantEveryoneInherited makes dir behave like %ProgramData%: everything
// created under it is readable by every account.
func grantEveryoneInherited(t *testing.T, dir string) {
	t.Helper()
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	// Merged with what the directory already grants, so the test can still
	// write there.
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	old, _, _ := sd.DACL()
	merged, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_READ,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID,
			TrusteeValue: windows.TrusteeValueFromSID(everyone)},
	}}, old)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION, nil, nil, merged, nil); err != nil {
		t.Fatal(err)
	}
}

// A state directory made under %ProgramData% inherits "every user may read".
// Before ProtectStateDir locked it down, the credential written there was
// readable by the accounts the agent records.
func TestTheStateDirectoryIsPrivateUnderAPublicParent(t *testing.T) {
	parent := t.TempDir()
	grantEveryoneInherited(t, parent)

	plain := filepath.Join(parent, "plain")
	if err := os.MkdirAll(plain, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plain, "secret"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if secrets.CheckPrivate(filepath.Join(plain, "secret")) == nil {
		t.Fatal("the test's parent does not make files public, so it proves nothing")
	}

	state := filepath.Join(parent, "Argus")
	if err := ProtectStateDir(state); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(state, "enrollment.json")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := secrets.CheckPrivate(p); err != nil {
		t.Fatalf("a file in the protected state directory is not private: %v", err)
	}
}

func TestOnlyLocalAccountsAreLoggedOn(t *testing.T) {
	computer, err := windows.ComputerName()
	if err != nil {
		t.Fatal(err)
	}
	for _, ok := range []string{"ana", `.\ana`, computer + `\ana`, strings.ToLower(computer) + `\ana`} {
		u, d, err := splitAccount(ok)
		if err != nil || u != "ana" || d != computer {
			t.Errorf("%q: %q %q %v", ok, u, d, err)
		}
	}
	for _, bad := range []string{`CORP\ana`, "ana@corp.example", "", `.\`, "a/b"} {
		if _, _, err := splitAccount(bad); err == nil {
			t.Errorf("%q was accepted as a local account", bad)
		}
	}
}

// The local accounts read in passwd form, each with a home that is a
// Windows path.
func TestTheLocalAccountsReadWithTheirHomes(t *testing.T) {
	accts, err := readAccounts("")
	if err != nil {
		t.Fatal(err)
	}
	if len(accts) == 0 {
		t.Fatal("no enabled local accounts were listed")
	}
	for _, a := range accts {
		if !strings.Contains(a.home, `:\`) {
			t.Errorf("%s has home %q", a.name, a.home)
		}
	}
}
