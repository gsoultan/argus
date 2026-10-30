//go:build !windows

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gsoultan/argus/internal/agent"
)

const sshdConfig = "/etc/ssh/sshd_config"

// removeSSHDHook comments out the agent's ForceCommand in sshd_config and its
// drop-ins, checks sshd still accepts the result, and reloads it. A config
// sshd rejects is put back as it was, and the uninstall stops: an sshd that
// cannot start is a host nobody can log in to. left is what a person still has
// to do before plain SSH works.
func removeSSHDHook() (notes, left []string, err error) {
	files := []string{sshdConfig}
	dropIns, _ := filepath.Glob(filepath.Join(filepath.Dir(sshdConfig), "sshd_config.d", "*.conf"))
	files = append(files, dropIns...)

	type original struct {
		path string
		data []byte
		mode os.FileMode
	}
	var changed []original
	restore := func() {
		for _, o := range changed {
			_ = writeFileAtomic(o.path, o.data, o.mode)
		}
	}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			restore()
			return nil, nil, fmt.Errorf("cannot read %s: %w", path, err)
		}
		out, n := disableArgusForceCommand(data)
		if n == 0 {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			restore()
			return nil, nil, err
		}
		if err := writeFileAtomic(path, out, info.Mode().Perm()); err != nil {
			restore()
			return nil, nil, fmt.Errorf("cannot write %s: %w", path, err)
		}
		changed = append(changed, original{path, data, info.Mode().Perm()})
		notes = append(notes, fmt.Sprintf("commented out %d ForceCommand line(s) in %s", n, path))
	}

	sshd := sshdBinary()
	if sshd == "" {
		if len(changed) == 0 {
			return []string{"sshd is not installed here; nothing to change"}, nil, nil
		}
		return notes, []string{"sshd could not be found to check the change. Check and " +
			"reload it before relying on plain SSH: " + reloadSSHDHint}, nil
	}
	if out, err := exec.Command(sshd, "-t").CombinedOutput(); err != nil {
		restore()
		return nil, nil, fmt.Errorf("sshd rejects its configuration (%s), so it was put back "+
			"as it was: %s", err, strings.TrimSpace(string(out)))
	}
	// What sshd will actually do, Include directives and all. A hook in a file
	// read above is gone; one this did not read is still refusing logins.
	if out, err := exec.Command(sshd, "-T").Output(); err == nil {
		for _, line := range bytes.Split(out, []byte("\n")) {
			if isArgusForceCommand(string(line)) {
				restore()
				return nil, nil, fmt.Errorf("sshd still runs the agent for every login (%s), "+
					"from a file this did not read. Remove that line, then run this again",
					strings.TrimSpace(string(line)))
			}
		}
	}
	if len(changed) == 0 {
		return []string{"no ForceCommand runs the agent; sshd is unchanged"}, nil, nil
	}
	// sshd keeps the configuration it started with until it reloads, so until
	// then it still hands every login to the agent that is about to be gone.
	if done, err := reloadSSHD(); err != nil {
		left = append(left, fmt.Sprintf("Reload sshd (%v), or SSH logins stay refused "+
			"until it restarts: %s", err, reloadSSHDHint))
	} else {
		notes = append(notes, done)
	}
	return append(notes, "direct SSH logins are no longer refused or recorded"), left, nil
}

func sshdBinary() string {
	if p, err := exec.LookPath("sshd"); err == nil {
		return p
	}
	for _, p := range []string{"/usr/sbin/sshd", "/usr/local/sbin/sshd"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// writeFileAtomic replaces path so sshd never reads half a file.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".argus-uninstall.tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// remainingSSHWaysIn is sshWaysIn for this host. known is false when sshd is
// not installed or would not say what it does, and there is nothing to check.
func remainingSSHWaysIn() (ways []string, known bool) {
	sshd := sshdBinary()
	if sshd == "" {
		return nil, false
	}
	out, err := exec.Command(sshd, "-T").Output()
	if err != nil {
		return nil, false
	}
	p, _ := agent.Scan(agent.DefaultScanConfig(""))
	return sshWaysIn(parseSSHDT(string(out)), p.UnmanagedKeys), true
}
