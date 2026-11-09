package control

import (
	"bufio"
	"io"
	"os"

	"github.com/gsoultan/argus/internal/recorder"
)

// recordingSpool is a stored recording copied, as stored, to a temporary
// file, so it can be read more than once: verified, then served.
//
// It holds the stored bytes, which are compressed, not the recording: a
// desktop session that is gigabytes plain is tens of megabytes here, and it
// is on disk, not in memory. Each read decompresses as it goes, so what a
// replay costs in memory is the decoder's window, whatever the length of
// the session.
type recordingSpool struct{ f *os.File }

// spoolRecording copies src to a temporary file, removed on Close.
func spoolRecording(src io.Reader) (*recordingSpool, error) {
	f, err := os.CreateTemp("", "argus-replay-*")
	if err != nil {
		return nil, err
	}
	s := &recordingSpool{f: f}
	if _, err := io.Copy(f, src); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Read calls fn with the recording, decompressed, from its start.
func (s *recordingSpool) Read(fn func(plain io.Reader) error) error {
	if _, err := s.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	plain, err := recorder.Open(s.f)
	if err != nil {
		return err
	}
	defer plain.Close()
	return fn(bufio.NewReaderSize(plain, 64<<10))
}

// Head returns up to n bytes of the recording's start, decompressed.
func (s *recordingSpool) Head(n int) ([]byte, error) {
	var head []byte
	err := s.Read(func(plain io.Reader) error {
		head = make([]byte, n)
		got, err := io.ReadFull(plain, head)
		head = head[:got]
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			err = nil
		}
		return err
	})
	return head, err
}

// Close removes the spool.
func (s *recordingSpool) Close() {
	_ = s.f.Close()
	_ = os.Remove(s.f.Name())
}
