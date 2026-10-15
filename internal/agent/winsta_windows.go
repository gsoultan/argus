package agent

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

/*
A window station for each session.

Every Windows process, console ones included, attaches to a window station
and desktop when it starts; one that cannot fails with STATUS_DLL_INIT_FAILED
before running a line. The agent's own is the one LocalSystem services share,
which the account rightly cannot open, and Windows will not create one for a
token from an S4U network logon.

Granting the account the services' window station would put it on a desktop
with every LocalSystem service's windows. So each session gets a window
station and desktop of its own, open to SYSTEM and to that session's logon
SID only, and closed with it.
*/

var (
	modUser32                   = windows.NewLazySystemDLL("user32.dll")
	procCreateWindowStationW    = modUser32.NewProc("CreateWindowStationW")
	procGetProcessWindowStation = modUser32.NewProc("GetProcessWindowStation")
	procSetProcessWindowStation = modUser32.NewProc("SetProcessWindowStation")
	procCreateDesktopW          = modUser32.NewProc("CreateDesktopW")
	procCloseDesktop            = modUser32.NewProc("CloseDesktop")
	procCloseWindowStation      = modUser32.NewProc("CloseWindowStation")
)

const (
	winstaAllAccess  = 0x37F | windows.STANDARD_RIGHTS_REQUIRED
	desktopAllAccess = 0x1FF | windows.STANDARD_RIGHTS_REQUIRED
)

// winstaMu serialises the moment the process switches window station:
// CreateDesktop makes the desktop on the process's current one.
var winstaMu sync.Mutex

type sessionDesktop struct {
	winsta, desk uintptr
	// Name is what STARTUPINFO.lpDesktop takes: "station\desktop".
	Name string
}

func newSessionDesktop(token windows.Token) (*sessionDesktop, error) {
	logon, err := logonSID(token)
	if err != nil {
		return nil, err
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;SY)(A;;GA;;;" + logon + ")")
	if err != nil {
		return nil, err
	}
	sa := windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(sa))

	id := make([]byte, 8)
	_, _ = rand.Read(id)
	name := "argus-" + hex.EncodeToString(id)

	winsta, _, err := procCreateWindowStationW.Call(
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(name))), 0, winstaAllAccess,
		uintptr(unsafe.Pointer(&sa)))
	if winsta == 0 {
		return nil, fmt.Errorf("cannot create a window station for the session: %w", err)
	}

	winstaMu.Lock()
	old, _, _ := procGetProcessWindowStation.Call()
	var desk uintptr
	if r, _, serr := procSetProcessWindowStation.Call(winsta); r == 0 {
		err = serr
	} else {
		desk, _, err = procCreateDesktopW.Call(
			uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("Default"))), 0, 0, 0,
			desktopAllAccess, uintptr(unsafe.Pointer(&sa)))
		procSetProcessWindowStation.Call(old)
	}
	winstaMu.Unlock()
	if desk == 0 {
		procCloseWindowStation.Call(winsta)
		return nil, fmt.Errorf("cannot create a desktop for the session: %w", err)
	}
	return &sessionDesktop{winsta: winsta, desk: desk, Name: name + `\Default`}, nil
}

// Close releases the agent's handles; the station goes when the session's
// processes, which hold their own, have exited.
func (d *sessionDesktop) Close() {
	if d == nil {
		return
	}
	procCloseDesktop.Call(d.desk)
	procCloseWindowStation.Call(d.winsta)
}

// logonSID is the SID of the logon session the token belongs to, which
// every process of this session carries and no other session's does.
func logonSID(token windows.Token) (string, error) {
	groups, err := token.GetTokenGroups()
	if err != nil {
		return "", err
	}
	for _, g := range groups.AllGroups() {
		if g.Attributes&windows.SE_GROUP_LOGON_ID == windows.SE_GROUP_LOGON_ID {
			return g.Sid.String(), nil
		}
	}
	return "", errors.New("the session's token has no logon SID")
}
