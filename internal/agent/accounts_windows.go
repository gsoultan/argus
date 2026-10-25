package agent

import (
	"bytes"
	"fmt"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// userInfo1 is USER_INFO_1.
type userInfo1 struct {
	Name        *uint16
	Password    *uint16
	PasswordAge uint32
	Priv        uint32
	HomeDir     *uint16
	Comment     *uint16
	Flags       uint32
	ScriptPath  *uint16
}

const (
	ufAccountDisable   = 0x0002
	filterNormalAcct   = 0x0002
	maxPreferredLength = 0xFFFFFFFF
	errMoreData        = 234
)

// directoryPasswd lists the enabled local accounts in passwd(5) form, with
// the home every one of them gets and the shell a session would start, so
// the readers shared with Unix see a Windows host's accounts.
func directoryPasswd() ([]byte, error) {
	var buf bytes.Buffer
	shell := filepath.Base(loginShell())
	var resume uint32
	for {
		var p *byte
		var read, total uint32
		err := windows.NetUserEnum(nil, 1, filterNormalAcct, &p, maxPreferredLength,
			&read, &total, &resume)
		if err != nil && err != windows.Errno(errMoreData) {
			return nil, fmt.Errorf("cannot list local accounts: %w", err)
		}
		if p != nil {
			for _, u := range unsafe.Slice((*userInfo1)(unsafe.Pointer(p)), read) {
				if u.Flags&ufAccountDisable != 0 {
					continue
				}
				name := windows.UTF16PtrToString(u.Name)
				fmt.Fprintf(&buf, "%s:*:0:0::%s:%s\n", name, profileHome(name), shell)
			}
			windows.NetApiBufferFree(p)
		}
		if err == nil {
			return buf.Bytes(), nil
		}
	}
}

// profileHome is the account's profile directory: where it is, if the
// account has logged on, else where it will be.
func profileHome(name string) string {
	sid, _, _, err := windows.LookupSID("", name)
	if err == nil {
		if dir := profileDirOf(sid.String()); dir != "" {
			return dir
		}
	}
	return filepath.Join(profilesRoot(), name)
}
