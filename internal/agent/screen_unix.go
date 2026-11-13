//go:build linux || darwin

package agent

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// helperArgs is the helper's command line after the binary.
func helperArgs(mode helperMode) []string {
	args := []string{"screen-helper"}
	if mode.gdiOnly {
		args = append(args, "--gdi")
	}
	if mode.video {
		args = append(args, "--video")
	}
	return args
}

// startHelperCmd starts cmd with pipes for its stdin and stdout, and nothing
// else open to it. cmd must start a process group of its own (Setpgid), so
// that ending the helper ends whatever it started.
func startHelperCmd(cmd *exec.Cmd) (io.ReadWriteCloser, error) {
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, err
	}
	growPipe(outW)
	cmd.Stdin, cmd.Stdout = inR, outW
	if err := cmd.Start(); err != nil {
		for _, f := range []*os.File{inR, inW, outR, outW} {
			f.Close()
		}
		return nil, fmt.Errorf("starting the screen helper: %w", err)
	}
	inR.Close()
	outW.Close()
	p := &unixHelperPipe{r: outR, w: inW, cmd: cmd, exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(p.exited)
	}()
	return p, nil
}

// screenPipeBuffer is the size of the pipe the screen comes through, where
// the system lets it be set: a whole batch of tiles, as on Windows.
const screenPipeBuffer = 1 << 20

// unixHelperPipe is the helper's stdin and stdout, and the process.
type unixHelperPipe struct {
	r, w   *os.File
	cmd    *exec.Cmd
	exited chan struct{}
	once   sync.Once
}

func (p *unixHelperPipe) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *unixHelperPipe) Write(b []byte) (int, error) { return p.w.Write(b) }

// Close ends the helper: the end of its stdin tells it to stop, letting go
// of any key it holds; a moment later its process group is killed, whatever
// it is doing. An ending session must not wait on the helper.
func (p *unixHelperPipe) Close() error {
	p.once.Do(func() {
		_ = p.w.Close()
		select {
		case <-p.exited:
		case <-time.After(screenHelperExit):
			_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
			<-p.exited
		}
		_ = p.r.Close()
	})
	return nil
}

// screenHelperExit is how long a helper told to stop has before it is ended.
const screenHelperExit = time.Second
