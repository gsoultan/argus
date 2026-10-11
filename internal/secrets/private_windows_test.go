//go:build windows

package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// keyWithReaders writes a key whose access list is exactly the given SIDs
// with read access, inheritance cut so the temp directory's list does not
// leak in.
func keyWithReaders(t *testing.T, sids ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	var entries []windows.EXPLICIT_ACCESS
	for _, s := range sids {
		sid, err := windows.StringToSid(s)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.GENERIC_READ,
			AccessMode:        windows.GRANT_ACCESS,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		})
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(p, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	return p
}

func me(t *testing.T) string {
	t.Helper()
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return tu.User.Sid.String()
}

const (
	sidSystem   = "S-1-5-18"
	sidAdmins   = "S-1-5-32-544"
	sidEveryone = "S-1-1-0"
	sidAuthUser = "S-1-5-11"
	sidUsers    = "S-1-5-32-545"
)

// The mode bits say nothing on Windows: Go reports 0666 for any writable
// file. A key the process alone can read must still load.
func TestCheckPrivateAcceptsAKeyOnlyItsOwnerAndAdministratorsCanRead(t *testing.T) {
	if err := CheckPrivate(keyWithReaders(t, me(t), sidSystem, sidAdmins)); err != nil {
		t.Fatalf("an owner-only key was refused: %v", err)
	}
}

// What a key copied into C:\ProgramData inherits: every local user can read
// it. That is the Windows "chmod 644", and it is refused.
func TestCheckPrivateRefusesAKeyOtherAccountsCanRead(t *testing.T) {
	for _, broad := range []string{sidEveryone, sidAuthUser, sidUsers} {
		err := CheckPrivate(keyWithReaders(t, me(t), sidSystem, broad))
		if err == nil {
			t.Errorf("a key readable by %s was accepted", broad)
			continue
		}
		if !strings.Contains(err.Error(), "readable") || !strings.Contains(err.Error(), "icacls") {
			t.Errorf("%s: the refusal should say why and how to fix it: %v", broad, err)
		}
	}
}

func TestCheckPrivateRefusesANullAccessList(t *testing.T) {
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A null DACL grants everyone everything.
	if err := windows.SetNamedSecurityInfo(p, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivate(p); err == nil {
		t.Fatal("a key with no access list was accepted")
	}
}

func TestCheckPrivateRefusesDirectoryAndMissingOnWindows(t *testing.T) {
	if err := CheckPrivate(t.TempDir()); err == nil {
		t.Error("a directory must be refused")
	}
	if err := CheckPrivate(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("a missing file must be refused")
	}
}
