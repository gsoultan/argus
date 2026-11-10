package agent

import (
	"testing"

	"github.com/gsoultan/argus/internal/rdp"
)

// A typed character goes as a press and release of each UTF-16 unit, so an
// emoji, outside the BMP, arrives whole as its surrogate pair.
func TestDesktopEventsTypeCharacters(t *testing.T) {
	ev, ok := desktopEvents(desktopInput{Type: "char", Char: "é"})
	if !ok || len(ev) != 2 || ev[0].Kind != "unicode" || ev[0].Unicode != 0xE9 || !ev[0].Down || ev[1].Down {
		t.Fatalf("é = %+v ok=%v", ev, ok)
	}
	ev, _ = desktopEvents(desktopInput{Type: "char", Char: "😀"})
	if len(ev) != 4 || ev[0].Unicode != 0xD83D || ev[2].Unicode != 0xDE00 {
		t.Fatalf("emoji = %+v, want its surrogate pair pressed and released", ev)
	}
	if _, ok := desktopEvents(desktopInput{Type: "char"}); ok {
		t.Error("an empty character was sent")
	}
}

func TestDesktopEventsCarryLocksAndDropUnknownKeys(t *testing.T) {
	ev, ok := desktopEvents(desktopInput{Type: "sync", Locks: rdp.LockCaps | 0xF0})
	if !ok || len(ev) != 1 || ev[0].Kind != "sync" || ev[0].Locks != rdp.LockCaps {
		t.Fatalf("sync = %+v ok=%v, want caps only", ev, ok)
	}
	if _, ok := desktopEvents(desktopInput{Type: "key", Code: "BrowserSearch", Down: true}); ok {
		t.Error("an unmapped key was sent")
	}
	ev, ok = desktopEvents(desktopInput{Type: "key", Code: "IntlBackslash", Down: true})
	if !ok || ev[0].Scancode != 0x56 {
		t.Errorf("IntlBackslash = %+v ok=%v", ev, ok)
	}
}
