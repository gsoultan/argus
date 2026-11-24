package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"
	"time"
)

// WithLocal is a bucket that recordings are moving into from the directory
// this host kept them in before the bucket was configured.
//
// Without it, turning object storage on would strand every recording made
// until then: the database names keys the new bucket does not have, and
// replay would answer "not found" for evidence that is sitting on this
// host's disk. Reads that the bucket cannot answer are answered from the
// directory instead, until CopyDir has copied everything across.
//
// Writes go to the bucket only. A delete removes both copies, because a
// recording someone removed from the console must not live on in the
// directory.
type WithLocal struct {
	Bucket
	Local *Dir
}

// Get opens key from the bucket, or from the directory when the bucket does
// not have it yet.
func (w *WithLocal) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	r, err := w.Bucket.Get(ctx, key)
	if err == nil || !IsNotFound(err) || w.Local == nil {
		return r, err
	}
	if lr, lerr := w.Local.Get(ctx, key); lerr == nil {
		return lr, nil
	}
	return nil, err
}

// Delete removes key from the bucket and from the directory.
func (w *WithLocal) Delete(ctx context.Context, key string) error {
	err := w.Bucket.Delete(ctx, key)
	if w.Local != nil {
		err = errors.Join(err, w.Local.Delete(ctx, key))
	}
	return err
}

// HeadFunc computes the chain head of the recording r holds. The storage
// package moves bytes and knows nothing of what they are, so the caller
// supplies it (recorder.Verify).
type HeadFunc func(r io.Reader) (string, error)

// CopyResult is what one CopyDir pass did.
type CopyResult struct {
	Copied  int // copied to the bucket on this pass
	Already int // already in the bucket at the same size
	Failed  int // could not be copied; left for the next pass
	Bytes   int64
}

// CopyDir copies every recording in d that b does not hold at the same size,
// under the same key, with its chain head as metadata. The directory's copies
// are left where they are: removing evidence from this host is an
// operator's decision, not a side effect of configuring a bucket.
//
// A recording whose head cannot be computed is copied anyway, without the
// metadata: a copy of evidence that does not verify is still evidence, and
// replay checks it against the head in the database either way.
func CopyDir(ctx context.Context, d *Dir, b Bucket, head HeadFunc, log *slog.Logger) (CopyResult, error) {
	var res CopyResult
	if d == nil || b == nil {
		return res, ErrNotConfigured
	}
	if log == nil {
		log = slog.Default()
	}
	local, err := d.List(ctx)
	if err != nil {
		return res, err
	}
	if len(local) == 0 {
		return res, nil
	}
	remote, err := b.List(ctx)
	if err != nil {
		return res, err
	}
	have := make(map[string]int64, len(remote))
	for _, o := range remote {
		have[o.Key] = o.Size
	}
	for _, o := range local {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		if size, ok := have[o.Key]; ok && size == o.Size {
			res.Already++
			continue
		}
		p, err := d.path(o.Key)
		if err != nil {
			res.Failed++
			continue
		}
		chainHead := ""
		if head != nil {
			if r, err := d.Get(ctx, o.Key); err == nil {
				chainHead, err = head(r)
				r.Close()
				if err != nil {
					log.Warn("copying a recording whose chain head could not be computed",
						"key", o.Key, "error", err)
				}
			}
		}
		if _, err := b.UploadKey(ctx, p, o.Key, sessionOf(o.Key), chainHead, startedOf(o.Key)); err != nil {
			res.Failed++
			log.Warn("could not copy a recording to object storage; it stays on this host and is tried again",
				"key", o.Key, "error", err)
			continue
		}
		res.Copied++
		res.Bytes += o.Size
	}
	if res.Failed > 0 {
		return res, fmt.Errorf("%d recordings could not be copied", res.Failed)
	}
	return res, nil
}

// sessionOf is the session id a key names: the file name without its
// extensions.
func sessionOf(key string) string {
	base := path.Base(key)
	if i := strings.IndexByte(base, '.'); i > 0 {
		return base[:i]
	}
	return base
}

// startedOf is the day a key's date prefix names, or the zero time for a key
// that has none (a database backup).
func startedOf(key string) time.Time {
	parts := strings.Split(path.Dir(key), "/")
	if len(parts) < 3 {
		return time.Time{}
	}
	t, err := time.Parse("2006/01/02", strings.Join(parts[len(parts)-3:], "/"))
	if err != nil {
		return time.Time{}
	}
	return t
}
