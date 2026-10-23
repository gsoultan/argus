package agent

import (
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

A failure is one line on stderr and exit status 1.
*/

// RunFileOp is the file-op subcommand.
func RunFileOp(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fail := func(err error) int {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	if len(args) < 2 {
		return fail(errors.New("usage: file-op list|download|upload PATH [-f]"))
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
		overwrite := len(args) > 2 && args[2] == "-f"
		if err := writeFile(path, stdin, overwrite); err != nil {
			return fail(err)
		}
	default:
		return fail(fmt.Errorf("unknown operation %q", args[0]))
	}
	return 0
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
