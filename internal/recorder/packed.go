package recorder

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/klauspost/compress/zstd"
)

/*
Packed: a desktop recording's base64 taken back out before zstd sees it.

A desktop recording is newline-delimited JSON, and each screen update is a
line [t,"s","<base64>"]. Base64 is what JSON needs, and what the chain, the
live tap and every reader are built on, so the recording keeps it. But it
is what zstd is worst at: three bytes of pixels become four characters, so
the same pixels a few tiles later sit at another offset in the alphabet and
no longer match. Measured on a synthetic 5-minute 1080p desktop through the
real screen encoder (226 MB plain):

	                    as recorded        packed
	zstd better/32 MB   34.1 MB  6.6x      10.6 MB  21.3x   (Hot)
	zstd best/128 MB    18.5 MB  12.2x      8.9 MB  25.3x   (Archive)

and packed compresses two and a half times faster and decompresses faster,
because there is less to do. Splitting the payloads into a stream of their
own gained nothing measurable; xz -9 on the packed stream is 5.3 MB but at
2 MB/s, and a new dependency besides.

So a recording with such lines is packed inside the zstd frame: each line
that is exactly [t,"s"|"c","<canonical base64>"] becomes its time, stream
and raw bytes, and every other line is kept as it is. Unpacking writes the
same bytes back, so the lines the chain covers, and the head they verify
to, are unchanged; CompressFile and the archive both read the copy back
before anything is removed. A recording with no such line (a terminal, a
database session) is not packed at all, and stays a plain zstd file anyone
can read with the zstd tool.

The stream, after its magic:

	0x00 uvarint(n) <n bytes>                       a line, as it is
	0x01 uvarint(n) <time> <stream> uvarint(m) <m>  [<time>,"<stream>","<base64 of m>"]

with 0x80 set on either when no newline follows: the last line of a file
that does not end in one, or a line too long to pack in one piece, written
in parts.
*/

// packedMagic opens a packed stream. A recording's plain bytes never start
// with a NUL.
var packedMagic = []byte("\x00argus-packed-1\n")

const (
	recLine   = 0x00
	recFrame  = 0x01
	recNoLF   = 0x80
	packChunk = 4 << 20
	// maxRecord bounds what an unpacker allocates for one record, so a
	// hostile file cannot make it allocate more.
	maxRecord = 16 << 20
)

var strictBase64 = base64.StdEncoding.Strict()

// packable reports whether any whole line in b would pack.
func packable(b []byte) bool {
	for {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			return false
		}
		if _, _, _, ok := splitFrame(b[:i], nil); ok {
			return true
		}
		b = b[i+1:]
	}
}

// splitFrame takes [t,"s","<base64>"] apart. ok is set only when putting the
// parts back together gives exactly line. raw is decoded into scratch when
// it is large enough.
func splitFrame(line, scratch []byte) (t []byte, stream byte, raw []byte, ok bool) {
	if len(line) < 10 || line[0] != '[' || !bytes.HasSuffix(line, []byte(`"]`)) {
		return nil, 0, nil, false
	}
	c := bytes.IndexByte(line, ',')
	if c < 2 {
		return nil, 0, nil, false
	}
	rest := line[c:]
	if len(rest) < 8 || rest[1] != '"' || (rest[2] != 's' && rest[2] != 'c') || string(rest[3:6]) != `","` {
		return nil, 0, nil, false
	}
	b64 := rest[6 : len(rest)-2]
	max := base64.StdEncoding.DecodedLen(len(b64))
	raw = slices.Grow(scratch[:0], max)[:max]
	// Only the canonical encoding comes back byte for byte: padded, its
	// unused bits zero (strict), and nothing skipped (the length).
	n, err := strictBase64.Decode(raw, b64)
	if err != nil || n == 0 || base64.StdEncoding.EncodedLen(n) != len(b64) {
		return nil, 0, nil, false
	}
	raw = raw[:n]
	return line[1:c], rest[2], raw, true
}

// pack writes src to w packed, after the magic.
func pack(w io.Writer, src *bufio.Reader) error {
	bw := bufio.NewWriterSize(w, 256<<10)
	if _, err := bw.Write(packedMagic); err != nil {
		return err
	}
	var hdr, scratch []byte
	for {
		line, err := src.ReadSlice('\n')
		lf := len(line) > 0 && line[len(line)-1] == '\n'
		if lf {
			line = line[:len(line)-1]
		}
		if len(line) > 0 || lf {
			hdr = hdr[:0]
			if t, s, raw, ok := splitFrame(line, scratch); ok && lf {
				scratch = raw
				hdr = append(hdr, recFrame)
				hdr = binary.AppendUvarint(hdr, uint64(len(t)))
				hdr = append(hdr, t...)
				hdr = append(hdr, s)
				hdr = binary.AppendUvarint(hdr, uint64(len(raw)))
				line = raw
			} else {
				tag := byte(recLine)
				if !lf {
					tag |= recNoLF
				}
				hdr = append(hdr, tag)
				hdr = binary.AppendUvarint(hdr, uint64(len(line)))
			}
			if _, werr := bw.Write(hdr); werr != nil {
				return werr
			}
			if _, werr := bw.Write(line); werr != nil {
				return werr
			}
		}
		switch {
		case err == nil, errors.Is(err, bufio.ErrBufferFull):
			continue
		case err == io.EOF:
			return bw.Flush()
		default:
			return err
		}
	}
}

// unpacker reads a packed stream back as the recording it was made from.
type unpacker struct {
	r   *bufio.Reader
	buf []byte // the line being given out, reused
	raw []byte // a frame's bytes, reused
	out []byte
	err error
}

func newUnpacker(r *bufio.Reader) (*unpacker, error) {
	magic := make([]byte, len(packedMagic))
	if _, err := io.ReadFull(r, magic); err != nil || !bytes.Equal(magic, packedMagic) {
		return nil, errors.New("not a packed recording")
	}
	return &unpacker{r: r}, nil
}

func (u *unpacker) Read(p []byte) (int, error) {
	for len(u.out) == 0 {
		if u.err != nil {
			return 0, u.err
		}
		u.err = u.next()
	}
	n := copy(p, u.out)
	u.out = u.out[n:]
	return n, nil
}

// next decodes one record into out.
func (u *unpacker) next() error {
	tag, err := u.r.ReadByte()
	if err != nil {
		return err // io.EOF between records is the end
	}
	u.buf = u.buf[:0]
	switch tag &^ recNoLF {
	case recLine:
		if u.buf, err = u.readN(u.buf); err != nil {
			return err
		}
	case recFrame:
		u.buf = append(u.buf, '[')
		if u.buf, err = u.readN(u.buf); err != nil {
			return err
		}
		s, err := u.r.ReadByte()
		if err != nil {
			return unexpected(err)
		}
		if s != 's' && s != 'c' {
			return fmt.Errorf("packed recording: unknown stream %q", s)
		}
		u.buf = append(u.buf, ',', '"', s, '"', ',', '"')
		if u.raw, err = u.readN(u.raw[:0]); err != nil {
			return err
		}
		u.buf = base64.StdEncoding.AppendEncode(u.buf, u.raw)
		u.buf = append(u.buf, '"', ']')
	default:
		return fmt.Errorf("packed recording: unknown record 0x%02x", tag)
	}
	if tag&recNoLF == 0 {
		u.buf = append(u.buf, '\n')
	}
	u.out = u.buf
	return nil
}

// readN reads a uvarint length and that many bytes onto b.
func (u *unpacker) readN(b []byte) ([]byte, error) {
	n, err := binary.ReadUvarint(u.r)
	if err != nil {
		return b, unexpected(err)
	}
	if n > maxRecord {
		return b, fmt.Errorf("packed recording: a %d-byte record is over the %d-byte bound", n, maxRecord)
	}
	at := len(b)
	b = slices.Grow(b, int(n))[:at+int(n)]
	if _, err := io.ReadFull(u.r, b[at:]); err != nil {
		return b, unexpected(err)
	}
	return b, nil
}

func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

// EncodingPacked is what a control plane lists among its recording
// encodings when it reads a packed zstd recording.
const EncodingPacked = "zstd-packed"

// IsPacked reports whether r is a zstd recording packed inside.
func IsPacked(r io.Reader) (bool, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	head, _ := br.Peek(4)
	if !isZstd(head) {
		return false, nil
	}
	zr, err := zstd.NewReader(br, zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderLowmem(true), zstd.WithDecoderMaxWindow(maxWindow))
	if err != nil {
		return false, err
	}
	defer zr.Close()
	inner := make([]byte, len(packedMagic))
	n, err := io.ReadFull(zr, inner)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return false, err
	}
	return bytes.Equal(inner[:n], packedMagic), nil
}
