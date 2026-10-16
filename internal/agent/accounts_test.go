package agent

import (
	"io"
	"testing"
)

// What `dscl . -readall /Users ...` prints on a Mac: system accounts with
// several names wrapped onto indented lines, a user whose home has a space,
// and a record with no shell at all.
const dsclSample = `NFSHomeDirectory: /var/empty
PrimaryGroupID: 83
RecordName:
 _amavisd
 amavisd
UniqueID: 83
UserShell: /usr/bin/false
-
NFSHomeDirectory:
 /Users/Ana Lima
PrimaryGroupID: 20
RecordName: ana
UniqueID: 501
UserShell: /bin/zsh
-
NFSHomeDirectory: /var/root
PrimaryGroupID: 0
RecordName:
 root
 BUILTIN\Local System
UniqueID: 0
UserShell: /bin/sh
-
NFSHomeDirectory: /var/empty
RecordName: _noshell
UniqueID: 99
`

func TestAMacsAccountsReadAsAPasswdFile(t *testing.T) {
	got := string(dsclToPasswd([]byte(dsclSample)))
	want := "_amavisd:*:83:83::/var/empty:/usr/bin/false\n" +
		"ana:*:501:20::/Users/Ana Lima:/bin/zsh\n" +
		"root:*:0:0::/var/root:/bin/sh\n"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

// The readers then see a Mac's people, and drop the accounts that cannot log
// in, exactly as they do on Linux.
func TestAMacsLoginAccountsComeFromTheDirectory(t *testing.T) {
	dir := t.TempDir()
	passwd := write(t, dir, "passwd", string(dsclToPasswd([]byte(dsclSample))))
	got := loginAccounts(passwd, write(t, dir, "shells", "/bin/sh\n/bin/zsh\n"))
	if len(got) != 2 || got[0] != "ana" || got[1] != "root" {
		t.Fatalf("login accounts = %v, want [ana root]", got)
	}
	accts, err := readAccounts(passwd)
	if err != nil || len(accts) != 2 || accts[0].home != "/Users/Ana Lima" {
		t.Fatalf("readAccounts = %+v, %v", accts, err)
	}
}

// On macOS this runs dscl; on Linux it reads /etc/passwd.
func TestTheSystemAccountDatabaseReads(t *testing.T) {
	f, err := openPasswd(systemPasswd)
	if err != nil {
		t.Skipf("no account database here: %v", err)
	}
	defer f.Close()
	b, _ := io.ReadAll(f)
	if len(b) == 0 {
		t.Fatal("the account database is empty")
	}
}
