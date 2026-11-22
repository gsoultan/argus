package agent

import (
	"bytes"
	"context"
	"os/exec"
	"time"
)

// platformRun runs a read-only tool with a short timeout and a bounded
// output. A missing tool is an error like any other.
func platformRun(ctx context.Context, name string, args ...string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	var out bytes.Buffer
	cmd.Stdout = &limitedBuffer{buf: &out, max: maxMetricsBody}
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return out.String(), nil
}

// limitedBuffer keeps the first max bytes and drops the rest.
type limitedBuffer struct {
	buf *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.buf.Len(); room > 0 {
		if len(p) > room {
			l.buf.Write(p[:room])
		} else {
			l.buf.Write(p)
		}
	}
	return len(p), nil
}
