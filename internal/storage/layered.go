package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"strings"
	"time"
)

// Layered is where recordings are going (Primary), in front of the places
// they were kept before (Fallbacks): the control plane's own disk before a
// bucket was configured, the control plane's bucket before an organization
// set up its own, or an organization's previous bucket.
//
// Without it, every change of destination would strand the recordings made
// until then: the database names keys the new destination does not have,
// and replay would answer "not found" for evidence that still exists. Reads
// the primary cannot answer are answered from the fallbacks, in order,
// until CopyObjects has copied everything across.
//
// Writes go to the primary only. A delete removes every copy, because a
// recording someone removed from the console must not live on elsewhere.
type Layered struct {
	Primary   Store
	Fallbacks []Store
}

var _ Store = (*Layered)(nil)

// Upload stores a recording in the primary.
func (l *Layered) Upload(ctx context.Context, p, sessionID, chainHead string, at time.Time) (string, error) {
	return l.Primary.Upload(ctx, p, sessionID, chainHead, at)
}

// UploadKey stores a file at key in the primary.
func (l *Layered) UploadKey(ctx context.Context, p, key, sessionID, chainHead string, at time.Time) (string, error) {
	ku, ok := l.Primary.(keyUploader)
	if !ok {
		return "", fmt.Errorf("%T cannot store an object under a key", l.Primary)
	}
	return ku.UploadKey(ctx, p, key, sessionID, chainHead, at)
}

// UploadKeyWith is UploadKey with options, for the archive.
func (l *Layered) UploadKeyWith(ctx context.Context, p, key, sessionID, chainHead string, at time.Time,
	opt PutOptions) (string, error) {
	au, ok := l.Primary.(archiveUploader)
	if !ok {
		return "", fmt.Errorf("%T cannot archive a recording", l.Primary)
	}
	return au.UploadKeyWith(ctx, p, key, sessionID, chainHead, at, opt)
}

// Get opens key from the primary, or from the first fallback that has it.
func (l *Layered) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	r, err := l.Primary.Get(ctx, key)
	if err == nil || !IsNotFound(err) {
		return r, err
	}
	for _, f := range l.Fallbacks {
		if fr, ferr := f.Get(ctx, key); ferr == nil {
			return fr, nil
		}
	}
	return nil, err
}

// List is what the primary holds, then what only a fallback holds: a backup
// copies all of it.
func (l *Layered) List(ctx context.Context) ([]Object, error) {
	out, err := l.Primary.List(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(out))
	for _, o := range out {
		seen[o.Key] = true
	}
	for _, f := range l.Fallbacks {
		more, err := f.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range more {
			if !seen[o.Key] {
				seen[o.Key] = true
				out = append(out, o)
			}
		}
	}
	return out, nil
}

// Delete removes key from the primary and from every fallback.
func (l *Layered) Delete(ctx context.Context, key string) error {
	var errs []error
	for _, s := range append([]Store{l.Primary}, l.Fallbacks...) {
		d, ok := s.(interface {
			Delete(context.Context, string) error
		})
		if !ok {
			continue
		}
		errs = append(errs, d.Delete(ctx, key))
	}
	return errors.Join(errs...)
}

// HeadFunc computes the chain head of the recording r holds. The storage
// package moves bytes and knows nothing of what they are, so the caller
// supplies it (recorder.Verify).
type HeadFunc func(r io.Reader) (string, error)

// CopyResult is what one CopyObjects pass did.
type CopyResult struct {
	Copied  int // copied on this pass
	Already int // already in the destination at the same size
	Failed  int // could not be copied; left for the next pass
	Bytes   int64
}

// Putter is a destination CopyObjects can write a key to as it is.
type Putter interface {
	Store
	UploadKey(ctx context.Context, path, key, sessionID, chainHead string,
		startedAt time.Time) (string, error)
}

// CopyObjects copies every object in src that dst does not hold at the same
// size, under the same key, with its chain head as metadata. src is left as
// it is: removing evidence from where it was is an operator's decision, not
// a side effect of choosing somewhere new.
//
// An object whose head cannot be computed is copied anyway, without the
// metadata: a copy of evidence that does not verify is still evidence, and
// replay checks it against the head in the database either way. Database
// backups stored beside recordings are copied the same way.
func CopyObjects(ctx context.Context, src Store, dst Putter, head HeadFunc, log *slog.Logger) (CopyResult, error) {
	var res CopyResult
	if src == nil || dst == nil {
		return res, ErrNotConfigured
	}
	if log == nil {
		log = slog.Default()
	}
	from, err := src.List(ctx)
	if err != nil {
		return res, err
	}
	if len(from) == 0 {
		return res, nil
	}
	to, err := dst.List(ctx)
	if err != nil {
		return res, err
	}
	have := make(map[string]int64, len(to))
	for _, o := range to {
		have[o.Key] = o.Size
	}
	for _, o := range from {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		if size, ok := have[o.Key]; ok && size == o.Size {
			res.Already++
			continue
		}
		if err := copyOne(ctx, src, dst, o.Key, head, log); err != nil {
			res.Failed++
			log.Warn("could not copy a stored object; it stays where it is and is tried again",
				"key", o.Key, "error", err)
			continue
		}
		res.Copied++
		res.Bytes += o.Size
	}
	if res.Failed > 0 {
		return res, fmt.Errorf("%d objects could not be copied", res.Failed)
	}
	return res, nil
}

// copyOne copies key through a temporary file: an upload needs the size
// first, and the head is computed from the same bytes that are sent.
func copyOne(ctx context.Context, src Store, dst Putter, key string, head HeadFunc, log *slog.Logger) error {
	r, err := src.Get(ctx, key)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "argus-copy-*")
	if err != nil {
		_ = r.Close()
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = io.Copy(tmp, r)
	_ = r.Close()
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	chainHead := ""
	if head != nil && isRecording(key) {
		if f, err := os.Open(tmp.Name()); err == nil {
			chainHead, err = head(f)
			f.Close()
			if err != nil {
				log.Warn("copying a recording whose chain head could not be computed", "key", key, "error", err)
			}
		}
	}
	_, err = dst.UploadKey(ctx, tmp.Name(), key, sessionOf(key), chainHead, startedOf(key))
	return err
}

// isRecording reports whether key names a recording rather than another
// object kept beside them, such as a database backup.
func isRecording(key string) bool {
	for _, ext := range []string{".cast", ".argusrdp", ".argussql"} {
		if strings.HasSuffix(key, ext) || strings.HasSuffix(key, ext+compressedExt) {
			return true
		}
	}
	return false
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
