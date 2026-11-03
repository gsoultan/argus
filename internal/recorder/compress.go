package recorder

import (
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/klauspost/compress/zstd"
)

/*
Recordings at rest are zstd.

A recording is written plain, line by line, while the session runs: that is
what leaves a replayable prefix behind a killed process. Once it is sealed it
is compressed, and stays compressed on the host, in the bucket and in the
archive.

The hash chain covers the recording's lines, not the file's bytes, so a
compressed copy verifies to exactly the head the session was sealed with:
Verify reads through the compression, and so does every reader in the
product. Nothing about the evidence changes; only what it costs to keep.

Why zstd, and these levels (measured on a synthetic 1080p desktop session and
on real terminal recordings; see the PR that introduced this file):

  - Hot is what an agent writes when a session is sealed and what the control
    plane stores when an older agent ships a plain file. It has to keep up on
    a busy host, so it is zstd's "better" level with a 32 MB window: a desktop
    recording shrinks around ten times, a terminal one five to ten.
  - Archive is what the control plane rewrites a recording into once it is
    older than the archive age. Nobody is waiting on it, so it is the best
    level with a 128 MB window, which finds the screens and output a session
    repeats minutes apart.

The decoder is bounded to the archive window, so a hostile file cannot make a
reader allocate more than that.
*/

// CompressedExt is appended to a recording's own extension once it is
// compressed: <id>.cast.zst, <id>.argusrdp.zst.
const CompressedExt = ".zst"

// Level is how hard a recording is compressed.
type Level int

const (
	// Hot is for a recording just sealed: fast enough for the host it was
	// made on.
	Hot Level = iota
	// Archive is for a recording nobody is waiting on.
	Archive
)

const (
	hotWindow     = 32 << 20
	archiveWindow = 128 << 20
	// maxWindow bounds what a reader will allocate for one stream.
	maxWindow = archiveWindow
)

var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

// Compressed reports whether the first bytes of a stream are zstd or gzip:
// what Open would decompress.
func Compressed(head []byte) bool {
	return isZstd(head) || isGzip(head)
}

func isZstd(b []byte) bool { return len(b) >= 4 && string(b[:4]) == string(zstdMagic) }
func isGzip(b []byte) bool { return len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b }

// Open returns the recording r carries, decompressed when it is zstd (what
// Argus writes) or gzip (what agents before zstd left on hosts), and as it
// is otherwise.
func Open(r io.Reader) (io.ReadCloser, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	head, _ := br.Peek(4)
	switch {
	case isZstd(head):
		zr, err := zstd.NewReader(br,
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderLowmem(true),
			zstd.WithDecoderMaxWindow(maxWindow))
		if err != nil {
			return nil, fmt.Errorf("open compressed recording: %w", err)
		}
		return zstdReadCloser{zr}, nil
	case isGzip(head):
		zr, err := gzip.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("open compressed recording: %w", err)
		}
		return zr, nil
	}
	return io.NopCloser(br), nil
}

type zstdReadCloser struct{ *zstd.Decoder }

func (z zstdReadCloser) Close() error { z.Decoder.Close(); return nil }

// Compress writes src to dst as zstd at the given level.
func Compress(dst io.Writer, src io.Reader, lvl Level) error {
	opts := []zstd.EOption{
		zstd.WithEncoderLevel(zstd.SpeedBetterCompression),
		zstd.WithWindowSize(hotWindow),
		// One core: on an agent this runs beside the sessions it records.
		zstd.WithEncoderConcurrency(1),
	}
	if lvl == Archive {
		opts = []zstd.EOption{
			zstd.WithEncoderLevel(zstd.SpeedBestCompression),
			zstd.WithWindowSize(archiveWindow),
			zstd.WithEncoderConcurrency(1),
		}
	}
	zw, err := zstd.NewWriter(dst, opts...)
	if err != nil {
		return err
	}
	if _, err := io.Copy(zw, src); err != nil {
		zw.Close()
		return err
	}
	return zw.Close()
}

// CompressedName is the name a recording at path has once compressed.
func CompressedName(path string) string {
	if strings.HasSuffix(path, CompressedExt) {
		return path
	}
	return path + CompressedExt
}

// ErrRoundTrip means a compressed copy did not read back to the original.
// It never should; it is checked because the original is evidence and is
// removed on the strength of the copy.
var ErrRoundTrip = errors.New("the compressed copy does not read back to the original")

// CompressFile replaces the plain recording at path with path.zst and
// returns the new path.
//
// The original goes only once the copy is complete, synced, and has been
// read back to the same bytes: a crash or a fault in between leaves the
// original, and at worst both, never neither.
func CompressFile(path string, lvl Level) (string, error) {
	in, err := os.Open(path)
	if err != nil {
		return "", err
	}
	// Closed before the original is removed: Windows will not delete a file
	// that is still open.
	defer func() {
		if in != nil {
			_ = in.Close()
		}
	}()
	head := make([]byte, 4)
	n, _ := io.ReadFull(in, head)
	if Compressed(head[:n]) {
		return path, nil
	}
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return "", err
	}

	dst := CompressedName(path)
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	plain := sha256.New()
	err = Compress(out, io.TeeReader(in, plain), lvl)
	if serr := out.Sync(); err == nil {
		err = serr
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = sameAs(tmp, plain.Sum(nil))
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	_ = in.Close()
	in = nil
	if err := os.Remove(path); err != nil {
		return dst, err
	}
	return dst, nil
}

// sameAs reads the compressed file at path back and compares it with the
// digest of the original.
func sameAs(path string, want []byte) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r, err := Open(f)
	if err != nil {
		return err
	}
	defer r.Close()
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return fmt.Errorf("%w: %v", ErrRoundTrip, err)
	}
	if string(h.Sum(nil)) != string(want) {
		return ErrRoundTrip
	}
	return nil
}
