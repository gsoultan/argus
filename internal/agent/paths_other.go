//go:build !darwin && !windows

package agent

// Where the agent keeps things on Linux. systemd's RuntimeDirectory and
// StateDirectory create the first two; the package creates the rest.
const (
	socketPath = "/run/argus/collector.sock"

	// DefaultStateDir holds the enrollment, posture, heartbeat and spool.
	DefaultStateDir = "/var/lib/argus"
	// DefaultRecordingsDir is where recordings are written, root-only.
	DefaultRecordingsDir = "/var/lib/argus/recordings"
	// DefaultShimPath is what sshd's ForceCommand invokes.
	DefaultShimPath = "/usr/libexec/argus-shim"

	// systemPasswd is the account database. Empty would mean "ask the
	// directory service", which only macOS needs.
	systemPasswd  = "/etc/passwd"
	osReleasePath = "/etc/os-release"
	machineIDPath = "/etc/machine-id"

	sshdConfigPath = "/etc/ssh/sshd_config"
	// adminKeysFile is an authorized_keys file outside any home; none here.
	adminKeysFile = ""

	// sessionLang is a UTF-8 locale every install has.
	sessionLang = "C.UTF-8"
)
