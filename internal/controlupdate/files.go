// Package controlupdate is the control plane updating itself: the files the
// control plane and its update helper pass between them, and the helper.
//
// The control plane runs as the unprivileged argus account in a sandbox and
// cannot install a package, and it is not given the means. It writes a
// request naming a version into its own state directory; a root unit watching
// that file (argus-control-update.path) starts the helper, which downloads
// and checks the package itself, takes a backup, installs it, restarts the
// control plane and waits for the new version to say it is ready. A version
// that does not come up is replaced by the one that was running.
//
//	argus-control (argus)            argus-control-update (root)
//	  update/request.json  ───────▶   reads the version, nothing else
//	  update/status.json   ◀───────   every step, dated
//	  update/ready.json    ───────▶   the new version: listening, database
//
// Everything in update/ is written by argus and read by root, so the helper
// treats it as untrusted: it takes only a version from the request, checks
// its shape, downloads from the address in its own root-owned configuration,
// keeps packages in a directory of its own, and never follows a link there.
package controlupdate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// File names in the update directory.
const (
	RequestFile = "request.json"
	StatusFile  = "status.json"
	ReadyFile   = "ready.json"
)

// States, as the console reads them.
const (
	StateWaiting     = "waiting"     // the control plane waits for sessions that would end
	StateRequested   = "requested"   // handed to the helper, which has not answered yet
	StateDownloading = "downloading" // the helper fetches and checks the package
	StateBackingUp   = "backing_up"
	StateInstalling  = "installing"
	StateRestarting  = "restarting" // installed; waiting for the new version to be ready
	StateRollingBack = "rolling_back"
	StateDone        = "done"
	StateRolledBack  = "rolled_back"
	StateFailed      = "failed"
	StateCancelled   = "cancelled"
)

// Finished reports whether state is an end.
func Finished(state string) bool {
	switch state {
	case StateDone, StateRolledBack, StateFailed, StateCancelled:
		return true
	}
	return false
}

// Request is the control plane asking for an update.
type Request struct {
	ID          string    `json:"id"`
	Version     string    `json:"version"`
	From        string    `json:"from"`
	RequestedBy string    `json:"requested_by"`
	At          time.Time `json:"at"`
}

// Step is one thing the update did, for the console's list.
type Step struct {
	State   string    `json:"state"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

// Status is where an update is, written by the helper and read by the
// control plane on both sides of the restart.
type Status struct {
	ID          string    `json:"id"`
	From        string    `json:"from"`
	Target      string    `json:"target"`
	RequestedBy string    `json:"requested_by,omitempty"`
	State       string    `json:"state"`
	Message     string    `json:"message"`
	Steps       []Step    `json:"steps,omitempty"`
	At          time.Time `json:"at"`
	// Audited is set by the control plane once it has recorded the outcome,
	// so a restart does not record it twice.
	Audited bool `json:"audited,omitempty"`
}

// Ready is a control plane saying it is serving.
type Ready struct {
	Version   string    `json:"version"`
	PID       int       `json:"pid"`
	At        time.Time `json:"at"`
	Listening bool      `json:"listening"`
	Database  bool      `json:"database"`
}

// reVersion is a release version: digits, dots and an optional pre-release
// label. Nothing else reaches a URL or a file name.
var reVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]{1,32})?$`)

// ValidVersion reports whether v is shaped like a release version.
func ValidVersion(v string) bool { return reVersion.MatchString(v) }

// readSmall reads a regular file of at most 64 KB without following a link,
// because the directory is writable by an account the reader does not trust.
func readSmall(path string) ([]byte, error) {
	f, err := openNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return io.ReadAll(io.LimitReader(f, 64<<10))
}

// writeJSON replaces path with v by renaming a new file over it. The new file
// is created exclusively, so a link planted at its name is refused, and the
// rename replaces whatever is at path itself, never what a link there names.
func writeJSON(path string, v any, mode os.FileMode) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(append(b, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, mode)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

func readJSON(path string, v any) error {
	b, err := readSmall(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// ReadRequest reads the request in dir.
func ReadRequest(dir string) (Request, error) {
	var r Request
	if err := readJSON(filepath.Join(dir, RequestFile), &r); err != nil {
		return Request{}, err
	}
	if !ValidVersion(r.Version) {
		return Request{}, fmt.Errorf("the request names %q, which is not a release version", r.Version)
	}
	if len(r.ID) == 0 || len(r.ID) > 64 {
		return Request{}, errors.New("the request has no id")
	}
	return r, nil
}

// WriteRequest hands a request to the helper.
func WriteRequest(dir string, r Request) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, RequestFile), r, 0o600)
}

// ReadStatus reads the last update's status in dir.
func ReadStatus(dir string) (Status, bool) {
	var s Status
	if err := readJSON(filepath.Join(dir, StatusFile), &s); err != nil {
		return Status{}, false
	}
	return s, true
}

// WriteStatus replaces the status in dir. The helper writes it as root; it is
// left readable and writable by the directory's owner, the control plane,
// which marks it audited.
func WriteStatus(dir string, s Status) error {
	if err := writeJSON(filepath.Join(dir, StatusFile), s, 0o600); err != nil {
		return err
	}
	return chownLike(filepath.Join(dir, StatusFile), dir)
}

// WriteReady says this control plane is serving.
func WriteReady(dir string, r Ready) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, ReadyFile), r, 0o600)
}

// ReadReady reads what the running control plane last said about itself.
func ReadReady(dir string) (Ready, bool) {
	var r Ready
	if err := readJSON(filepath.Join(dir, ReadyFile), &r); err != nil {
		return Ready{}, false
	}
	return r, true
}
