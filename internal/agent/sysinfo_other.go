//go:build !linux

package agent

// Outside Linux only what the Go runtime knows is reported (architecture,
// processor count, time zone); there is no /proc to read the rest from.
func platformSystem() SystemInfo { return SystemInfo{} }

func (u *UsageSampler) platformSample() (Usage, bool) { return Usage{}, false }
