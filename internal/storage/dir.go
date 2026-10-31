package storage

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Store is where the control plane keeps the recordings agents ship: a bucket
// when object storage is configured, a directory on its own disk otherwise.
type Store interface {
	Upload(ctx context.Context, path, sessionID, chainHead string, startedAt time.Time) (string, error)
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	List(ctx context.Context) ([]Object, error)
}

var (
	_ Store = (*Client)(nil)
	_ Store = (*Dir)(nil)
)

// Dir keeps recordings in a directory on the control plane's own disk.
//
// What an install without object storage uses, so a recording can still be
// replayed from the console. It is a weaker home than a bucket the control
// plane cannot delete from -- whoever owns this host can remove the evidence
// -- but the chain head is in the database either way, so an altered file is
// still caught when it is read, and a missing one is noticed by the backup
// drill.
//
// Keys are the same as a bucket's, so a directory can later be copied into a
// bucket as it is and the database will still name the right objects.
type Dir struct {
	root string
}

// NewDir keeps recordings under root, creating it readable by this process
// alone.
func NewDir(root string) (*Dir, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("recordings directory: %w", err)
	}
	return &Dir{root: root}, nil
}

// Root is where the recordings are.
func (d *Dir) Root() string { return d.root }

// Upload copies the recording at path into the directory and returns its key.
func (d *Dir) Upload(_ context.Context, path, sessionID, _ string, startedAt time.Time) (string, error) {
	if d == nil {
		return "", ErrNotConfigured
	}
	ext := filepath.Ext(path)
	if ext == "" {
		ext = ".cast"
	}
	key := ObjectKeyExt(startedAt, sessionID, ext)
	dst, err := d.path(key)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", err
	}
	src, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open recording: %w", err)
	}
	defer src.Close()

	// Written beside and renamed into place, so a reader never sees half a
	// recording and a crash leaves no file the database could come to name.
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".upload-*")
	if err != nil {
		return "", err
	}
	_, err = io.Copy(tmp, src)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dst)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("store %s: %w", key, err)
	}
	return key, nil
}

// Get opens a recording for reading.
func (d *Dir) Get(_ context.Context, key string) (io.ReadCloser, error) {
	if d == nil {
		return nil, ErrNotConfigured
	}
	p, err := d.path(key)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

// List returns every recording in the directory.
func (d *Dir) List(_ context.Context) ([]Object, error) {
	if d == nil {
		return nil, ErrNotConfigured
	}
	var out []Object
	err := filepath.WalkDir(d.root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(d.root, p)
		if err != nil {
			return err
		}
		out = append(out, Object{Key: filepath.ToSlash(rel), Size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", d.root, err)
	}
	return out, nil
}

// path keeps a key inside the directory. Keys come from the database, and
// one that climbs out of it must not become a way to read any file this
// process can.
func (d *Dir) path(key string) (string, error) {
	rel := filepath.FromSlash(key)
	if key == "" || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("recording key %q is not inside the recordings directory", key)
	}
	return filepath.Join(d.root, rel), nil
}
