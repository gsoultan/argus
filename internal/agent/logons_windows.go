//go:build windows

package agent

import (
	"context"
	"errors"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/gsoultan/argus/internal/tunnel"
)

var (
	procWTSLogoffSession = modWtsapi32.NewProc("WTSLogoffSession")
)

const (
	wtsActive       = 0
	wtsConnected    = 1
	wtsDisconnected = 4

	wtsClientName  = 10
	wtsSessionInfo = 24

	verNTWorkstation = 1
)

// wtsInfo is WTSINFOW.
type wtsInfo struct {
	State                   int32
	SessionID               uint32
	IncomingBytes           uint32
	OutgoingBytes           uint32
	IncomingFrames          uint32
	OutgoingFrames          uint32
	IncomingCompressedBytes uint32
	OutgoingCompressedBytes uint32
	WinStationName          [32]uint16
	Domain                  [17]uint16
	UserName                [21]uint16
	ConnectTime             int64
	DisconnectTime          int64
	LastInputTime           int64
	LogonTime               int64
	CurrentTime             int64
}

type windowsLogons struct{}

func platformLogons() LogonsHost { return windowsLogons{} }

func (windowsLogons) List(context.Context) (tunnel.LogonsResult, error) {
	res := tunnel.LogonsResult{Platform: "windows"}
	res.Edition, res.RemoteLimit = windowsRemoteLimit()

	var list *windows.WTS_SESSION_INFO
	var n uint32
	if err := windows.WTSEnumerateSessions(0, 0, 1, &list, &n); err != nil {
		return res, errors.New("could not list the sessions: " + err.Error())
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(list)))
	console := windows.WTSGetActiveConsoleSessionId()
	for _, s := range unsafe.Slice(list, n) {
		if s.State != wtsActive && s.State != wtsConnected && s.State != wtsDisconnected {
			continue
		}
		user := wtsString(s.SessionID, wtsUserName)
		if user == "" {
			// The sign-in screen, waiting for someone: nobody to show.
			continue
		}
		account := user
		if d := wtsString(s.SessionID, wtsDomainName); d != "" {
			account = d + `\` + user
		}
		l := tunnel.Logon{Session: s.SessionID, Account: account, State: tunnel.LogonActive,
			Console: s.SessionID == console,
			Remote:  strings.HasPrefix(strings.ToUpper(windows.UTF16PtrToString(s.WindowStationName)), "RDP-"),
			From:    wtsString(s.SessionID, wtsClientName)}
		if s.State == wtsDisconnected {
			l.State = tunnel.LogonDisconnected
		}
		if info, ok := sessionInfo(s.SessionID); ok {
			if info.LogonTime > 0 {
				t := fileTime(info.LogonTime)
				l.LogonAt = &t
			}
			if info.LastInputTime > 0 && info.CurrentTime > info.LastInputTime {
				l.IdleSeconds = (info.CurrentTime - info.LastInputTime) / 10_000_000
			}
		}
		res.Logons = append(res.Logons, l)
	}
	return res, nil
}

func (windowsLogons) SignOut(_ context.Context, session uint32) error {
	// bWait: return once the session has ended, so the console's next list
	// no longer shows it.
	r, _, err := procWTSLogoffSession.Call(0, uintptr(session), 1)
	if r == 0 {
		return err
	}
	return nil
}

func sessionInfo(session uint32) (wtsInfo, bool) {
	var buf *wtsInfo
	var n uint32
	r, _, _ := procWTSQuerySessionInformationW.Call(0, uintptr(session), wtsSessionInfo,
		uintptr(unsafe.Pointer(&buf)), uintptr(unsafe.Pointer(&n)))
	if r == 0 || buf == nil || n < uint32(unsafe.Sizeof(wtsInfo{})) {
		return wtsInfo{}, false
	}
	defer procWTSFreeMemory.Call(uintptr(unsafe.Pointer(buf)))
	return *buf, true
}

// fileTime is a FILETIME (100ns since 1601) as a time.
func fileTime(ft int64) time.Time {
	const epochDiff = 116444736000000000
	return time.Unix(0, (ft-epochDiff)*100).UTC()
}

// windowsRemoteLimit says how many Remote Desktop sessions this edition
// takes at once: one on a desktop edition, two on a server that is not a
// Remote Desktop Session Host. 0 is unknown or no fixed limit.
func windowsRemoteLimit() (edition string, limit int) {
	v := windows.RtlGetVersion()
	if v.ProductType == verNTWorkstation {
		return "workstation", 1
	}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Terminal Server`, registry.QUERY_VALUE)
	if err == nil {
		defer k.Close()
		// TSAppCompat is 1 on a Session Host, which takes as many sessions as
		// it is licensed for.
		if appCompat, _, err := k.GetIntegerValue("TSAppCompat"); err == nil && appCompat == 1 {
			return "server", 0
		}
	}
	return "server", 2
}
