package agent

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
One file operation, as the account.

`argus-agent file-op` is what the files service runs for each request, started
as the session's account the same way a tunnel shell is. Nothing here runs as
root: what the account cannot read it cannot download, and what it cannot
write it cannot upload, exactly as for scp. The agent only carries the bytes
and keeps the record.

	file-op list     PATH         → JSON listing on stdout
	file-op download PATH         → the file's bytes on stdout
	file-op upload   PATH [-f]    → stdin written to PATH; -f replaces a file
	file-op mkdir    PATH         → a new folder; JSON answer on stdout
	file-op rename   PATH NEWPATH → moved, never over something that exists
	file-op delete   PATH         → a file, link or empty folder removed
	file-op preview  PATH start|end → up to MaxPreview bytes, as JSON

A failure is one line on stderr and exit status 1.
*/

// RunFileOp is the file-op subcommand.
func RunFileOp(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fail := func(err error) int {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	if len(args) < 2 {
		return fail(errors.New("usage: file-op list|download|upload|mkdir|rename|delete|preview PATH [ARG]"))
	}
	path, err := resolvePath(args[1])
	if err != nil {
		return fail(err)
	}
	switch args[0] {
	case "list":
		ans, err := listDir(path)
		if err != nil {
			return fail(err)
		}
		if err := json.NewEncoder(stdout).Encode(ans); err != nil {
			return fail(err)
		}
	case "download":
		f, err := os.Open(path)
		if err != nil {
			return fail(plainFileError(err))
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return fail(err)
		}
		if !fi.Mode().IsRegular() {
			return fail(fmt.Errorf("%s is not a regular file", path))
		}
		if _, err := io.Copy(stdout, f); err != nil {
			return fail(err)
		}
	case "upload":
		var overwrite, framed bool
		for _, a := range args[2:] {
			overwrite = overwrite || a == "-f"
			framed = framed || a == "-framed"
		}
		in := stdin
		if framed {
			in = &uploadFrames{r: stdin}
		}
		if err := writeFile(path, in, overwrite); err != nil {
			return fail(err)
		}
	case "mkdir", "rename", "delete", "preview":
		ans, err := simpleOp(args[0], path, args[2:])
		if err != nil {
			return fail(err)
		}
		if err := json.NewEncoder(stdout).Encode(ans); err != nil {
			return fail(err)
		}
	default:
		return fail(fmt.Errorf("unknown operation %q", args[0]))
	}
	return 0
}

// simpleOp is one of the operations whose whole answer is one JSON line.
func simpleOp(op, path string, rest []string) (tunnel.FileAnswer, error) {
	ans := tunnel.FileAnswer{OK: true, Path: path}
	switch op {
	case "mkdir":
		if err := os.Mkdir(path, 0o755); err != nil {
			if errors.Is(err, fs.ErrExist) {
				return ans, fmt.Errorf("%s already exists", path)
			}
			return ans, plainFileError(err)
		}
	case "rename":
		if len(rest) < 1 || rest[0] == "" {
			return ans, errors.New("rename needs the new name")
		}
		to, err := resolvePath(rest[0])
		if err != nil {
			return ans, err
		}
		if err := renameNoReplace(path, to); err != nil {
			return ans, err
		}
		ans.To = to
	case "delete":
		if isRootish(path) {
			return ans, fmt.Errorf("%s is not deleted from the console", path)
		}
		fi, err := os.Lstat(path)
		if err != nil {
			return ans, plainFileError(err)
		}
		if err := os.Remove(path); err != nil {
			if fi.IsDir() {
				// Only an empty folder goes: a recursive delete from a browser
				// click is a mistake nobody can take back.
				return ans, fmt.Errorf("%s is not empty; delete what is in it first", path)
			}
			return ans, plainFileError(err)
		}
	case "preview":
		from := "start"
		if len(rest) > 0 {
			from = rest[0]
		}
		return previewFile(path, from)
	}
	return ans, nil
}

// renameNoReplace moves from to to, refusing when to already exists: a
// rename that silently replaced a file would be a delete nobody asked for.
// The check and the move are two steps, so a file appearing between them is
// replaced; where the platform offers it, the move itself refuses.
func renameNoReplace(from, to string) error {
	if _, err := os.Lstat(from); err != nil {
		return plainFileError(err)
	}
	if _, err := os.Lstat(to); err == nil {
		return fmt.Errorf("%s already exists", to)
	}
	if err := renameExclusive(from, to); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s already exists", to)
		}
		return plainFileError(err)
	}
	return nil
}

// isRootish is a path whose deletion the console never asks for.
func isRootish(p string) bool {
	clean := filepath.Clean(p)
	if clean == "/" || clean == "." {
		return true
	}
	home, _ := os.UserHomeDir()
	return home != "" && clean == filepath.Clean(home)
}

// previewFile reads the start or the end of a regular file, at most
// MaxPreview bytes, for the console to show without a download.
func previewFile(path, from string) (tunnel.FileAnswer, error) {
	ans := tunnel.FileAnswer{OK: true, Path: path}
	f, err := os.Open(path)
	if err != nil {
		return ans, plainFileError(err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ans, err
	}
	if !fi.Mode().IsRegular() {
		return ans, fmt.Errorf("%s is not a regular file", path)
	}
	ans.Size = fi.Size()
	var off int64
	if from == "end" && fi.Size() > tunnel.MaxPreview {
		off = fi.Size() - tunnel.MaxPreview
	}
	buf := make([]byte, tunnel.MaxPreview)
	n, err := f.ReadAt(buf, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return ans, err
	}
	ans.Content = buf[:n]
	ans.Truncated = fi.Size() > int64(n)
	return ans, nil
}

// resolvePath makes p absolute against the account's home.
func resolvePath(p string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "/"
	}
	switch {
	case p == "" || p == "~":
		return home, nil
	case strings.HasPrefix(p, "~/"):
		return filepath.Join(home, p[2:]), nil
	case filepath.IsAbs(p):
		return filepath.Clean(p), nil
	}
	return filepath.Join(home, p), nil
}

func listDir(path string) (tunnel.FileAnswer, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return tunnel.FileAnswer{}, plainFileError(err)
	}
	ans := tunnel.FileAnswer{OK: true, Path: path}
	for _, e := range entries {
		if len(ans.Entries) >= tunnel.MaxListing {
			ans.Truncated = true
			break
		}
		fe := tunnel.FileEntry{Name: e.Name(), Dir: e.IsDir()}
		if fi, err := e.Info(); err == nil {
			fe.Size = fi.Size()
			fe.Mode = fi.Mode().String()
			fe.ModTime = fi.ModTime().UTC().Format(time.RFC3339)
			if fi.Mode()&fs.ModeSymlink != 0 {
				fe.Link, _ = os.Readlink(filepath.Join(path, e.Name()))
				// Followed for the one question a browser needs answered:
				// can this be opened as a folder.
				if st, err := os.Stat(filepath.Join(path, e.Name())); err == nil {
					fe.Dir = st.IsDir()
				}
			}
		}
		ans.Entries = append(ans.Entries, fe)
	}
	sort.Slice(ans.Entries, func(i, j int) bool {
		a, b := ans.Entries[i], ans.Entries[j]
		if a.Dir != b.Dir {
			return a.Dir
		}
		return a.Name < b.Name
	})
	return ans, nil
}

// errUploadCut is an upload whose stream ended without its closing frame:
// the person cancelled it, or the connection went. Nothing is written.
var errUploadCut = errors.New("the upload stopped before it finished, so nothing was written")

// uploadFrames reads an upload the agent sends as length-prefixed chunks, a
// 4-byte big-endian length then that many bytes, ending with a zero length.
// Only that last frame makes it complete: a plain end of input means the
// stream was cut, which a bare byte stream could not tell from the end of
// the file, and a cut upload must never be renamed into place.
type uploadFrames struct {
	r    io.Reader
	left uint32
	done bool
}

func (u *uploadFrames) Read(p []byte) (int, error) {
	for u.left == 0 {
		if u.done {
			return 0, io.EOF
		}
		var hdr [4]byte
		if _, err := io.ReadFull(u.r, hdr[:]); err != nil {
			return 0, errUploadCut
		}
		u.left = binary.BigEndian.Uint32(hdr[:])
		if u.left == 0 {
			u.done = true
		}
	}
	if uint32(len(p)) > u.left {
		p = p[:u.left]
	}
	n, err := u.r.Read(p)
	u.left -= uint32(n)
	if err == io.EOF {
		if u.left > 0 || n == 0 {
			return n, errUploadCut
		}
		err = nil
	}
	if err != nil {
		return n, errUploadCut
	}
	return n, nil
}

// writeUploadFrame sends one chunk to the upload helper, length first; an
// empty chunk is the end of the file. See uploadFrames.
func writeUploadFrame(w io.Writer, p []byte) error {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(p)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(p) == 0 {
		return nil
	}
	_, err := w.Write(p)
	return err
}

// writeFile writes r to path through a temporary file beside it, so a
// transfer that stops halfway never leaves a truncated file where the old
// one was.
func writeFile(path string, r io.Reader, overwrite bool) error {
	if fi, err := os.Lstat(path); err == nil {
		if !overwrite {
			return fmt.Errorf("%s already exists", path)
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file, so it is not replaced", path)
		}
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".argus-upload-*")
	if err != nil {
		return plainFileError(err)
	}
	defer os.Remove(tmp.Name())
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if !overwrite {
		// A link fails if the name has appeared since the check above, where
		// a rename would replace it.
		err := os.Link(tmp.Name(), path)
		if err == nil {
			return nil
		}
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s already exists", path)
		}
	}
	return os.Rename(tmp.Name(), path)
}

// plainFileError says what went wrong without Go's "open /x: " prefix
// doubled up, since the path is shown beside it anyway.
func plainFileError(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %s", pe.Path, pe.Err)
	}
	return err
}
