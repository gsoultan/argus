package agent

// Where the agent keeps things on Windows. Everything is under
// %ProgramData%\Argus, which the agent locks to SYSTEM and Administrators
// (see ProtectStateDir) before it writes anything there.
const (
	socketPath = `C:\ProgramData\Argus\collector.sock`

	// DefaultStateDir holds the enrollment, posture, heartbeat and spool.
	DefaultStateDir = `C:\ProgramData\Argus`
	// DefaultRecordingsDir is where recordings are written.
	DefaultRecordingsDir = `C:\ProgramData\Argus\recordings`
	// DefaultShimPath is unused: the sshd hook is not ported yet.
	DefaultShimPath = ""

	// systemPasswd is empty: accounts come from the local account database.
	systemPasswd  = ""
	osReleasePath = ""
	machineIDPath = ""

	sshdConfigPath = `C:\ProgramData\ssh\sshd_config`
	// adminKeysFile is where Windows' OpenSSH reads the keys of every
	// member of Administrators, instead of their homes.
	adminKeysFile = `C:\ProgramData\ssh\administrators_authorized_keys`

	sessionLang = ""
)
