package main

import (
	"os"
	"sync"
)

// rotatingFile is a log file that starts afresh when it reaches max bytes,
// keeping the previous one as <path>.1.
//
// The Windows service logs to a file because it has no console. It used to
// rotate only when the service started, so an agent that ran for a year wrote
// a year of log into one file; this bounds it at twice max while running.
type rotatingFile struct {
	path string
	max  int64

	mu   sync.Mutex
	f    *os.File
	size int64
}

func openRotating(path string, max int64) (*rotatingFile, error) {
	r := &rotatingFile{path: path, max: max}
	if fi, err := os.Stat(path); err == nil && fi.Size() >= max {
		_ = os.Rename(path, path+".1")
	}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *rotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	r.f, r.size = f, fi.Size()
	return nil
}

// File is the file being written now, for what writes to a file directly.
func (r *rotatingFile) File() *os.File {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f
}

func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size > 0 && r.size+int64(len(p)) > r.max {
		// Windows will not rename a file that is open, so close it first.
		// If reopening fails, keep writing to the old name rather than
		// losing the line.
		_ = r.f.Close()
		_ = os.Rename(r.path, r.path+".1")
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}
