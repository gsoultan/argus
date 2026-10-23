package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// Where there is no recorder to point at, any ForceCommand matched the empty
// path, so a host whose sshd ran something else was reported as recorded.
func TestAForeignForceCommandIsNotTheRecorderWhenThereIsNoShimPath(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "sshd_config")
	if err := os.WriteFile(cfg, []byte("ForceCommand /usr/local/bin/other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	passwd := filepath.Join(dir, "passwd")
	if err := os.WriteFile(passwd, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := Scan(ScanConfig{SSHDConfig: cfg, PasswdFile: passwd, ShimPath: ""})
	if err != nil {
		t.Fatal(err)
	}
	if p.ShimInstalled {
		t.Fatal("a ForceCommand that is not the recorder was reported as the recorder")
	}
	if len(p.Drift) == 0 {
		t.Error("the replaced ForceCommand was not reported as drift")
	}
}

// Windows homes carry a drive letter, whose colon must not shift the shell
// into another field.
func TestAPasswdLineWithAWindowsHomeKeepsItsFields(t *testing.T) {
	f := passwdFields(`ana:*:0:0::C:\Users\ana:cmd.exe`)
	if len(f) != 7 || f[5] != `C:\Users\ana` || f[6] != "cmd.exe" {
		t.Fatalf("fields %q", f)
	}
	f = passwdFields("root:x:0:0:root:/root:/bin/bash")
	if len(f) != 7 || f[5] != "/root" || f[6] != "/bin/bash" {
		t.Fatalf("a Unix line changed: %q", f)
	}
}
