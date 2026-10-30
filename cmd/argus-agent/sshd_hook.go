package main

import (
	"bytes"
	"path/filepath"
	"strings"
)

// sshdHookMarker starts a line an uninstall commented out, so a person
// reading sshd_config later can see what was there and why it is not.
const sshdHookMarker = "# Removed by argus-agent uninstall: "

// disableArgusForceCommand comments out every ForceCommand that runs the
// agent, wherever it is (a Match block included), and returns how many.
//
// Only lines whose command is the agent itself: someone else's ForceCommand is
// theirs, and removing it would open whatever it was closing.
func disableArgusForceCommand(config []byte) ([]byte, int) {
	lines := bytes.SplitAfter(config, []byte("\n"))
	n := 0
	for i, line := range lines {
		if !isArgusForceCommand(string(line)) {
			continue
		}
		indent := line[:len(line)-len(bytes.TrimLeft(line, " \t"))]
		lines[i] = append(append([]byte(string(indent)), sshdHookMarker...),
			bytes.TrimLeft(line, " \t")...)
		n++
	}
	return bytes.Join(lines, nil), n
}

// isArgusForceCommand reads one sshd_config line the way sshd does: a keyword,
// case-insensitive, then whitespace or an equals sign, then the command.
func isArgusForceCommand(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return false
	}
	i := strings.IndexAny(line, " \t=")
	if i < 0 || !strings.EqualFold(line[:i], "ForceCommand") {
		return false
	}
	value := strings.TrimLeft(line[i:], " \t=")
	value = strings.Trim(firstField(value), `"`)
	return strings.HasPrefix(filepath.Base(value), "argus")
}

func firstField(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return ""
}
