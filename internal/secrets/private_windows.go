//go:build windows

package secrets

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ownerOnly refuses a file whose access list lets anyone read it but its
// owner, the account this process runs as, SYSTEM and the Administrators
// group -- the Windows reading of "not readable by group or others".
//
// It fails closed: an access list it cannot read, a missing or null one
// (which grants everyone everything), or an entry it does not understand is a
// refusal, not a pass.
func ownerOnly(path string, _ os.FileInfo) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("cannot read the access list of %s: %w", path, err)
	}
	dacl, _, err := sd.DACL()
	if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) || (err == nil && dacl == nil) {
		return fmt.Errorf("%s has no access list, so everyone can read it; "+
			"a private key must be readable only by its owner (%s)", path, fixHint(path))
	}
	if err != nil {
		return fmt.Errorf("cannot read the access list of %s: %w", path, err)
	}

	trusted, err := trustedReaders(sd)
	if err != nil {
		return fmt.Errorf("cannot check who may read %s: %w", path, err)
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return fmt.Errorf("cannot read the access list of %s: %w", path, err)
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue // applies to children, and a file has none
		}
		switch ace.Header.AceType {
		case windows.ACCESS_ALLOWED_ACE_TYPE:
		case windows.ACCESS_DENIED_ACE_TYPE, deniedObjectAce, deniedCallbackAce, deniedCallbackObjectAce:
			continue // a denial grants nothing
		default:
			return fmt.Errorf("%s has an access entry of type %d that Argus cannot "+
				"evaluate, so it cannot confirm the key is private (%s)",
				path, ace.Header.AceType, fixHint(path))
		}
		if ace.Mask&readAccess == 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if isAny(sid, trusted) {
			continue
		}
		return fmt.Errorf("%s is readable by %s; a private key must not be readable "+
			"by anyone but its owner and administrators (%s)", path, sidName(sid), fixHint(path))
	}
	return nil
}

// The denial types x/sys/windows has no names for (winnt.h).
const (
	deniedObjectAce         = 0x6
	deniedCallbackAce       = 0xA
	deniedCallbackObjectAce = 0xC
)

// readAccess is every right that lets a holder read the file's contents.
const readAccess = windows.FILE_READ_DATA | windows.GENERIC_READ | windows.GENERIC_ALL |
	windows.MAXIMUM_ALLOWED

// trustedReaders is who may read a private key: whoever could read it anyway.
func trustedReaders(sd *windows.SECURITY_DESCRIPTOR) ([]*windows.SID, error) {
	var out []*windows.SID
	if owner, _, err := sd.Owner(); err == nil && owner != nil {
		out = append(out, owner)
	}
	// The account this process runs as, which need not own the file -- a
	// service account reading a key an administrator placed, as a container
	// uid differs from a file's owner on Linux.
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	out = append(out, tu.User.Sid)
	for _, t := range []windows.WELL_KNOWN_SID_TYPE{
		windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid,
	} {
		s, err := windows.CreateWellKnownSid(t)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func isAny(sid *windows.SID, set []*windows.SID) bool {
	for _, s := range set {
		if sid.Equals(s) {
			return true
		}
	}
	return false
}

func sidName(sid *windows.SID) string {
	if account, domain, _, err := sid.LookupAccount(""); err == nil {
		if domain != "" {
			return domain + `\` + account
		}
		return account
	}
	return sid.String()
}

func fixHint(path string) string {
	return fmt.Sprintf(`icacls "%s" /inheritance:r /grant:r "%%USERNAME%%:R" /grant:r SYSTEM:F /grant:r Administrators:F`, path)
}
