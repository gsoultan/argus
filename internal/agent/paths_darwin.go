//go:build darwin

package agent

// Where the agent keeps things on macOS.
//
// /var/db is where macOS daemons keep state; /var/run is cleared at boot and
// the collector recreates its directory. The shim lives under /usr/local
// because System Integrity Protection makes /usr/libexec read-only even to
// root.
const (
	socketPath = "/var/run/argus/collector.sock"

	// DefaultStateDir holds the enrollment, posture, heartbeat and spool.
	DefaultStateDir = "/var/db/argus"
	// DefaultRecordingsDir is where recordings are written, root-only.
	DefaultRecordingsDir = "/var/db/argus/recordings"
	// DefaultShimPath is what sshd's ForceCommand invokes.
	DefaultShimPath = "/usr/local/bin/argus-agent"

	// systemPasswd is empty: macOS keeps accounts in Directory Services, and
	// /etc/passwd lists only the handful used in single-user mode. An empty
	// path means "ask dscl"; see openPasswd.
	systemPasswd = ""
	// macOS has neither; osName and machineID ask sw_vers and ioreg instead.
	osReleasePath = ""
	machineIDPath = ""

	// sessionLang is a UTF-8 locale every install has.
	sessionLang = "en_US.UTF-8"
)
