package agent

import (
	"fmt"
	"os"
	"sync"

	"golang.org/x/sys/windows"
)

// ProtectStateDir creates dir and replaces its access list with one that
// lets only SYSTEM and Administrators in, inherited by everything created
// under it.
//
// %ProgramData% lets every local user read, and create files in, a new
// directory; mode bits mean nothing here. Without this the credential and
// the recordings would be readable by the accounts being recorded.
//
// Setting the list walks everything already under dir, so it is done once
// per directory per process; sessions call this on every start.
func ProtectStateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if _, done := protectedDirs.Load(dir); done {
		return nil
	}
	if err := protectDir(dir); err != nil {
		return err
	}
	protectedDirs.Store(dir, struct{}{})
	return nil
}

// protectedDirs holds the directories already protected. Its keys come from
// the agent's own flags, not from callers, so it stays small.
var protectedDirs sync.Map

func protectDir(dir string) error {
	var entries []windows.EXPLICIT_ACCESS
	for _, t := range []windows.WELL_KNOWN_SID_TYPE{
		windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid,
	} {
		sid, err := windows.CreateWellKnownSid(t)
		if err != nil {
			return err
		}
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.SET_ACCESS,
			Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		})
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return err
	}
	// Protected: what the parent grants no longer flows in.
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		return fmt.Errorf("cannot restrict %s to SYSTEM and Administrators: %w", dir, err)
	}
	return nil
}

// The collector's socket lives in the protected state directory. Nothing
// on Windows connects to it yet -- the sshd hook is not ported -- so it is
// not opened to other users.
func prepareSocketDir(dir string) error { return ProtectStateDir(dir) }

func openSocket(string) error { return nil }
