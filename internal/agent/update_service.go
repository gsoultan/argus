package agent

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

/*
Updating an agent that is one executable run by a service manager: Windows.

There is no package. The release's archive holds argus-agent.exe, the agent
unpacked it and checked it before it launched this helper, and the plan
names that file, a copy of the binary that was running, the executable the
service starts, and the service.

The new binary goes in while the old one is still serving. Windows will not
let a running executable be written, but it will let it be renamed, so the
running file moves aside to <exe>.old and the new one takes its name. Only
then is the service stopped and started again, which is all the time logins
wait: the stop (the old agent, knowing it is restarting for an update,
disconnects within a couple of seconds) and the start.

If the new version does not say in ready.json that it is serving within
readyWait, or exits crashLoop times on the way, the binary that was running
goes back the same way and the service is restarted on it. Either way the
outcome is written to update.json, which the agent that comes up reads.
*/

// agentService is what the helper needs from the service manager.
type agentService interface {
	// Stop asks the service to stop and waits up to wait for it to, ending
	// its process if it has not.
	Stop(wait time.Duration) error
	// Start starts it. A service the manager has already started again is
	// not an error.
	Start() error
	// Process is the service's process id, 0 when it is not running.
	Process() uint32
}

// ServiceApplier carries out an update plan on a host whose agent is a
// single executable run by a service. It is what `argus-agent update-apply`
// runs on Windows, outside the service, as SYSTEM.
type ServiceApplier struct {
	Plan    UpdatePlan
	Log     *slog.Logger
	Service agentService
	// ReadyWait and StopWait override readyWait and serviceStopWait in tests.
	ReadyWait time.Duration
	StopWait  time.Duration
	// Poll is how often the service's process is looked at; 0 is a second.
	Poll time.Duration
	// HowToLook is the advice a failure ends with.
	HowToLook string
}

// serviceStopWait is how long the old agent has to stop. It seals what is
// open within updateDrain when it knows it is restarting for an update, but
// a stop that came some other way waits for its sessions, up to the 70
// seconds it tells the service manager.
const serviceStopWait = 90 * time.Second

// Apply puts the new version in, restarts the service, and checks it came
// back; if it did not, it puts the old binary back. It returns the state it
// ended in, which it has also written to update.json.
func (a *ServiceApplier) Apply() string {
	if a.Log == nil {
		a.Log = slog.Default()
	}
	p := a.Plan
	if p.Restart {
		return a.applyRestart()
	}
	status := UpdateStatus{ID: p.ID, From: p.From, Target: p.Target, State: UpdateRestarting,
		Message: "putting " + p.Target + " in place"}
	a.write(status)

	started := time.Now()
	reason := ""
	if err := a.place(p.Package); err != nil {
		reason = "could not put " + p.Target + " in place of the running agent: " + err.Error()
		// Nothing was stopped and the old binary is where it was.
		status.State, status.Message = UpdateFailed, reason+"; nothing was changed"
		a.finish(status)
		return UpdateFailed
	}
	status.Message = "restarting on " + p.Target
	a.write(status)
	if err := a.restart(); err != nil {
		reason = "the agent would not restart on " + p.Target + ": " + err.Error()
	} else if err := a.awaitReady(p.Target, started); err != nil {
		reason = p.Target + " " + err.Error()
	}
	if reason == "" {
		took := time.Since(started).Round(time.Second)
		status.State = UpdateDone
		status.Message = fmt.Sprintf("updated from %s to %s; back in %s", p.From, p.Target, took)
		a.finish(status)
		return UpdateDone
	}

	a.Log.Error("update: "+reason+"; going back", "to", p.From)
	status.Message = reason + "; going back to " + p.From
	a.write(status)
	back := time.Now()
	// Stopped first: a new version that is up but not serving still holds
	// its file, and one in a crash loop is restarted by the manager.
	err := a.Service.Stop(a.stopWait())
	if err == nil {
		err = a.place(p.Previous)
	}
	if err == nil {
		err = a.start()
	}
	if err == nil {
		err = a.awaitReady(p.From, back)
	}
	if err != nil {
		status.State = UpdateFailed
		status.Message = reason + ", and going back to " + p.From + " did not work either: " +
			err.Error() + ". " + a.howToLook()
		a.finish(status)
		return UpdateFailed
	}
	status.State = UpdateRolledBack
	status.Message = reason + "; went back to " + p.From
	a.finish(status)
	return UpdateRolledBack
}

// applyRestart restarts the service on the binary it runs and checks it
// came back. There is nothing to go back to.
func (a *ServiceApplier) applyRestart() string {
	p := a.Plan
	status := UpdateStatus{ID: p.ID, From: p.From, Target: p.Target, Restart: true,
		State: UpdateRestarting, Message: "restarting"}
	a.write(status)

	started := time.Now()
	err := a.restart()
	if err == nil {
		err = a.awaitReady(p.Target, started)
	} else {
		err = fmt.Errorf("would not restart: %w", err)
	}
	if err != nil {
		status.State = UpdateFailed
		status.Message = "the agent " + err.Error() + ". " + a.howToLook()
		a.finish(status)
		return UpdateFailed
	}
	status.State = UpdateDone
	status.Message = fmt.Sprintf("restarted; back in %s", time.Since(started).Round(time.Second))
	a.finish(status)
	return UpdateDone
}

// place puts src where the service's executable is, while that executable
// may be running: the running file is renamed aside, which Windows allows,
// and src is copied in under its name. If the copy cannot take the name,
// the running file is renamed back.
func (a *ServiceApplier) place(src string) error {
	exe := a.Plan.Executable
	if src == "" || exe == "" {
		return errors.New("the plan names no binary")
	}
	next := exe + ".new"
	if err := copyFile(src, next, 0o755); err != nil {
		return err
	}
	old := exe + ".old"
	// Left by an earlier update whose agent has since restarted, so no
	// longer running. If it is somehow still held, the rename below says so.
	_ = os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		_ = os.Remove(next)
		return err
	}
	if err := os.Rename(next, exe); err != nil {
		if rerr := os.Rename(old, exe); rerr != nil {
			return fmt.Errorf("%v, and the running agent could not be put back at %s: %v", err, exe, rerr)
		}
		_ = os.Remove(next)
		return err
	}
	return nil
}

func (a *ServiceApplier) restart() error {
	if err := a.Service.Stop(a.stopWait()); err != nil {
		return err
	}
	return a.start()
}

// start starts the service with the restart marker set, so logins arriving
// while it comes up wait for it.
func (a *ServiceApplier) start() error {
	if m := a.Plan.Marker; m != "" {
		_ = os.MkdirAll(filepath.Dir(m), 0o755)
		_ = os.WriteFile(m, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
	}
	return a.Service.Start()
}

// awaitReady waits for version to say it is serving. A process id that
// changes, or a service that is not running, is the manager restarting an
// agent that exited on its own: crashLoop of those and it is given up on.
func (a *ServiceApplier) awaitReady(version string, since time.Time) error {
	wait := a.ReadyWait
	if wait == 0 {
		wait = readyWait
	}
	poll := a.Poll
	if poll == 0 {
		poll = time.Second
	}
	var (
		crashes int
		last    = a.Service.Process()
		down    bool
		next    = time.Now()
	)
	return awaitAgentReady(a.Plan, version, since, wait, func() int {
		if time.Now().Before(next) {
			return crashes
		}
		next = time.Now().Add(poll)
		pid := a.Service.Process()
		switch {
		case pid == 0 && !down:
			// Exited; counted once, however long it stays down.
			down = true
			crashes++
		case pid != 0 && last != 0 && pid != last && !down:
			// Exited and was restarted between two looks.
			crashes++
		}
		if pid != 0 {
			down = false
			last = pid
		}
		return crashes
	})
}

func (a *ServiceApplier) stopWait() time.Duration {
	if a.StopWait > 0 {
		return a.StopWait
	}
	return serviceStopWait
}

func (a *ServiceApplier) howToLook() string {
	if a.HowToLook != "" {
		return a.HowToLook
	}
	return "Sign in to the server as an administrator and look at the agent's service and log"
}

func (a *ServiceApplier) write(s UpdateStatus) {
	s.At = time.Now().UTC()
	if err := WriteUpdateStatus(a.Plan.StateDir, s); err != nil {
		a.Log.Error("update: cannot write the update's state", "error", err)
	}
}

func (a *ServiceApplier) finish(s UpdateStatus) {
	a.write(s)
	if a.Plan.Marker != "" {
		_ = os.Remove(a.Plan.Marker)
	}
	// The binary that was running, now that the service runs another. Held
	// still only if the service is down, and then the next update removes it.
	if a.Plan.Executable != "" {
		_ = os.Remove(a.Plan.Executable + ".old")
	}
	for _, f := range []string{a.Plan.Package, a.Plan.Previous} {
		if f != "" {
			_ = os.Remove(f)
		}
	}
	if s.State == UpdateFailed {
		a.Log.Error("update: " + s.Message)
	} else {
		a.Log.Info("update: " + s.Message)
	}
}

// unpackFromZip writes the file called name, wherever it sits in the
// archive, into dir, and returns its path. The archive has been checked
// against the release's checksums; the bound is for an archive that is not
// what the release meant, not for one an attacker chose.
func unpackFromZip(archive, dir, name string) (string, error) {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return "", fmt.Errorf("cannot open the archive: %w", err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !strings.EqualFold(pathBase(f.Name), name) {
			continue
		}
		if f.UncompressedSize64 > maxPackage {
			return "", fmt.Errorf("%s in the archive is larger than any agent", name)
		}
		in, err := f.Open()
		if err != nil {
			return "", err
		}
		defer in.Close()
		path := filepath.Join(dir, name)
		out, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o700)
		if err != nil {
			return "", err
		}
		n, err := io.Copy(out, io.LimitReader(in, maxPackage+1))
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err == nil && n > maxPackage {
			err = fmt.Errorf("%s in the archive is larger than any agent", name)
		}
		if err != nil {
			_ = os.Remove(path)
			return "", err
		}
		return path, nil
	}
	return "", fmt.Errorf("the archive holds no %s", name)
}

// pathBase is the last element of a name in an archive, which may use
// either slash.
func pathBase(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		return name[i+1:]
	}
	return name
}
