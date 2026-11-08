//go:build windows

package agent

import (
	"context"
	"sort"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/gsoultan/argus/internal/tunnel"
)

var procWTSSendMessageW = modWtsapi32.NewProc("WTSSendMessageW")

const (
	mbOK            = 0x0
	mbYesNo         = 0x4
	mbIconQuest     = 0x20
	mbIconInfo      = 0x40
	mbDefButton2    = 0x100
	mbSystemModal   = 0x1000
	mbSetForeground = 0x10000
	idYes           = 6
	idTimeout       = 32000
)

type windowsAsker struct{}

func platformAsker() Asker { return windowsAsker{} }

func (windowsAsker) InUse(ctx context.Context) ([]tunnel.Logon, error) {
	res, err := windowsLogons{}.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []tunnel.Logon
	for _, l := range res.Logons {
		if l.State == tunnel.LogonActive {
			out = append(out, l)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Console && !out[j].Console })
	return out, nil
}

func sendMessage(session uint32, title, text string, style uint32, seconds int, wait bool) (uint32, error) {
	t, err := windows.UTF16FromString(title)
	if err != nil {
		return 0, err
	}
	m, err := windows.UTF16FromString(text)
	if err != nil {
		return 0, err
	}
	var resp uint32
	w := uintptr(0)
	if wait {
		w = 1
	}
	// Lengths are in bytes, without the terminating NUL.
	r, _, callErr := procWTSSendMessageW.Call(0, uintptr(session),
		uintptr(unsafe.Pointer(&t[0])), uintptr((len(t)-1)*2),
		uintptr(unsafe.Pointer(&m[0])), uintptr((len(m)-1)*2),
		uintptr(style), uintptr(seconds), uintptr(unsafe.Pointer(&resp)), w)
	if r == 0 {
		return 0, callErr
	}
	return resp, nil
}

func (windowsAsker) Ask(ctx context.Context, l tunnel.Logon, title, text string, seconds int) (bool, bool, error) {
	type answer struct {
		resp uint32
		err  error
	}
	done := make(chan answer, 1)
	go func() {
		resp, err := sendMessage(l.Session, title, text,
			mbYesNo|mbIconQuest|mbDefButton2|mbSystemModal|mbSetForeground, seconds, true)
		done <- answer{resp, err}
	}()
	select {
	case <-ctx.Done():
		// The box times out on its own; the session is not opened.
		return false, false, ctx.Err()
	case a := <-done:
		if a.err != nil {
			return false, false, a.err
		}
		if a.resp == idTimeout {
			return false, false, nil
		}
		return a.resp == idYes, true, nil
	}
}

func (windowsAsker) Tell(_ context.Context, l tunnel.Logon, title, text string) error {
	_, err := sendMessage(l.Session, title, text, mbOK|mbIconInfo|mbSystemModal, 60, false)
	return err
}
