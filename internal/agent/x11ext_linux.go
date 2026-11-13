//go:build linux

package agent

import (
	"fmt"
	"sync"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

/*
xgb's extension Init functions (shm.Init, damage.Init, ...) write the
event and error constructors into xgb's process-wide NewEventFuncs and
NewErrorFuncs maps on every call, while every open connection's reader
goroutine reads those maps, unlocked, for each event it receives. A second
connection, or reopening the screen, is a data race in xgb (the race
detector found it in TestX11HelperServesTheScreen).

x11InitExt does what Init does, but fills the shared maps only the first
time a given extension is seen at given event and error numbers, under one
lock; afterwards it only records the opcode on the connection, which xgb
guards with the connection's own ExtLock.
*/

var x11ExtMu sync.Mutex

// x11ExtDone is the extensions whose constructors are in xgb's maps, by
// name and the numbers they were given.
var x11ExtDone = map[string]bool{}

// x11InitExt makes extension name usable on c, as its package's Init does.
func x11InitExt(c *xgb.Conn, name string) error {
	reply, err := xproto.QueryExtension(c, uint16(len(name)), name).Reply()
	switch {
	case err != nil:
		return err
	case !reply.Present:
		return fmt.Errorf("the X server has no %s extension", name)
	}
	c.ExtLock.Lock()
	c.Extensions[name] = reply.MajorOpcode
	c.ExtLock.Unlock()

	key := fmt.Sprintf("%s/%d/%d", name, reply.FirstEvent, reply.FirstError)
	x11ExtMu.Lock()
	defer x11ExtMu.Unlock()
	if x11ExtDone[key] {
		return nil
	}
	x11ExtDone[key] = true
	for n, fun := range xgb.NewExtEventFuncs[name] {
		xgb.NewEventFuncs[int(reply.FirstEvent)+n] = fun
	}
	for n, fun := range xgb.NewExtErrorFuncs[name] {
		xgb.NewErrorFuncs[int(reply.FirstError)+n] = fun
	}
	return nil
}
