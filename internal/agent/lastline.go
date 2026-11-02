package agent

import "strings"

// lastLine is the last line of a tool's output, which is where it says why.
func lastLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
