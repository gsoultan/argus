package agent

// foregroundBusy cannot tell on a pseudoconsole: a held Windows terminal is
// kept for its hold and no longer.
func foregroundBusy(terminalIO, int) (busy, ok bool) { return false, false }
