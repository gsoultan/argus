package rdp

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// RDP 8.0 bulk decompression, which Windows calls ZGFX (MS-RDPEGFX 2.2.5,
// 3.1.9.1).
//
// Everything the server sends on the graphics channel is wrapped in it,
// compressed or not, so there is no reading the graphics pipeline without
// it. The history is shared across every message of a session: a match may
// reach back into a segment that arrived seconds ago, which is why one
// Decompressor belongs to one session and is never reset mid-stream.

// ErrZGFX reports a segment this decompressor could not expand.
var ErrZGFX = errors.New("malformed RDP8 bulk data")

const (
	zgfxSingle    = 0xE0
	zgfxMultipart = 0xE1

	zgfxTypeRDP8   = 0x04
	zgfxCompressed = 0x20

	// zgfxHistory is the sliding window the server compresses against.
	zgfxHistory = 2500000

	// zgfxMaxOutput bounds one message. A frame of 4K RGBA is 33 MB; nothing
	// on this channel is larger, and a length claimed beyond it is hostile.
	zgfxMaxOutput = 64 << 20
)

type zgfxToken struct {
	bits, code, valueBits int
	match                 bool
	base                  int
}

// zgfxTokens is the prefix code from MS-RDPEGFX 3.1.9.1.1, shortest first:
// literals carry a byte, matches a distance into the history.
var zgfxTokens = [...]zgfxToken{
	{1, 0b0, 8, false, 0},
	{5, 0b10001, 5, true, 0},
	{5, 0b10010, 7, true, 32},
	{5, 0b10011, 9, true, 160},
	{5, 0b10100, 10, true, 672},
	{5, 0b10101, 12, true, 1696},
	{5, 0b11000, 0, false, 0x00},
	{5, 0b11001, 0, false, 0x01},
	{6, 0b101100, 14, true, 5792},
	{6, 0b101101, 15, true, 22176},
	{6, 0b110100, 0, false, 0x02},
	{6, 0b110101, 0, false, 0x03},
	{6, 0b110110, 0, false, 0xFF},
	{7, 0b1011100, 18, true, 54944},
	{7, 0b1011101, 20, true, 317088},
	{7, 0b1101110, 0, false, 0x04},
	{7, 0b1101111, 0, false, 0x05},
	{7, 0b1110000, 0, false, 0x06},
	{7, 0b1110001, 0, false, 0x07},
	{7, 0b1110010, 0, false, 0x08},
	{7, 0b1110011, 0, false, 0x09},
	{7, 0b1110100, 0, false, 0x0A},
	{7, 0b1110101, 0, false, 0x0B},
	{7, 0b1110110, 0, false, 0x3A},
	{7, 0b1110111, 0, false, 0x3B},
	{7, 0b1111000, 0, false, 0x3C},
	{7, 0b1111001, 0, false, 0x3D},
	{7, 0b1111010, 0, false, 0x3E},
	{7, 0b1111011, 0, false, 0x3F},
	{7, 0b1111100, 0, false, 0x40},
	{7, 0b1111101, 0, false, 0x80},
	{8, 0b10111100, 20, true, 1365664},
	{8, 0b10111101, 21, true, 2414240},
	{8, 0b11111100, 0, false, 0x0C},
	{8, 0b11111101, 0, false, 0x38},
	{8, 0b11111110, 0, false, 0x39},
	{8, 0b11111111, 0, false, 0x66},
	{9, 0b101111100, 22, true, 4511392},
	{9, 0b101111101, 23, true, 8705696},
	{9, 0b101111110, 24, true, 17094304},
}

// Decompressor expands RDP_SEGMENTED_DATA from one session's graphics channel.
//
// Not safe for concurrent use, and not reusable across sessions: its history
// is the session's.
type Decompressor struct {
	history []byte
	at      int  // next write position in history
	full    bool // history has wrapped
}

// NewDecompressor returns a decompressor with an empty history.
func NewDecompressor() *Decompressor {
	return &Decompressor{history: make([]byte, zgfxHistory)}
}

// Decompress expands one RDP_SEGMENTED_DATA message.
func (z *Decompressor) Decompress(in []byte) ([]byte, error) {
	if len(in) < 1 {
		return nil, fmt.Errorf("%w: empty", ErrZGFX)
	}
	switch in[0] {
	case zgfxSingle:
		return z.segment(nil, in[1:])
	case zgfxMultipart:
		if len(in) < 7 {
			return nil, fmt.Errorf("%w: multipart header is truncated", ErrZGFX)
		}
		count := int(binary.LittleEndian.Uint16(in[1:3]))
		size := int(binary.LittleEndian.Uint32(in[3:7]))
		if size > zgfxMaxOutput {
			return nil, fmt.Errorf("%w: claims %d bytes", ErrZGFX, size)
		}
		out := make([]byte, 0, size)
		p := in[7:]
		for i := range count {
			if len(p) < 4 {
				return nil, fmt.Errorf("%w: segment %d of %d is missing", ErrZGFX, i+1, count)
			}
			n := int(binary.LittleEndian.Uint32(p[0:4]))
			if n > len(p)-4 {
				return nil, fmt.Errorf("%w: segment %d claims %d bytes, %d remain",
					ErrZGFX, i+1, n, len(p)-4)
			}
			var err error
			if out, err = z.segment(out, p[4:4+n]); err != nil {
				return nil, err
			}
			p = p[4+n:]
		}
		if len(out) != size {
			return nil, fmt.Errorf("%w: expanded to %d bytes, header said %d", ErrZGFX, len(out), size)
		}
		return out, nil
	}
	return nil, fmt.Errorf("%w: descriptor 0x%02X", ErrZGFX, in[0])
}

// segment appends one RDP8_BULK_ENCODED_DATA to out.
func (z *Decompressor) segment(out, seg []byte) ([]byte, error) {
	if len(seg) < 1 {
		return nil, fmt.Errorf("%w: segment has no header", ErrZGFX)
	}
	header, data := seg[0], seg[1:]
	if header&0x0F != zgfxTypeRDP8 {
		return nil, fmt.Errorf("%w: compression type 0x%X", ErrZGFX, header&0x0F)
	}
	if header&zgfxCompressed == 0 {
		z.remember(data)
		return append(out, data...), nil
	}
	if len(data) < 1 {
		return nil, fmt.Errorf("%w: compressed segment is empty", ErrZGFX)
	}

	// The last byte says how many bits of the one before it are padding.
	pad := int(data[len(data)-1])
	r := bitReader{data: data[:len(data)-1]}
	r.left = 8*len(r.data) - pad
	if pad > 7 || r.left < 0 {
		return nil, fmt.Errorf("%w: %d padding bits", ErrZGFX, pad)
	}

	start := len(out)
	for r.left > 0 {
		tok, err := r.token()
		if err != nil {
			return nil, err
		}
		if !tok.match {
			b := byte(tok.base)
			if tok.valueBits > 0 {
				v, err := r.bits(tok.valueBits)
				if err != nil {
					return nil, err
				}
				b = byte(tok.base + v)
			}
			out = append(out, b)
			z.remember(out[len(out)-1:])
			continue
		}
		v, err := r.bits(tok.valueBits)
		if err != nil {
			return nil, err
		}
		distance := tok.base + v
		if distance == 0 {
			// Raw bytes, aligned to the next whole byte.
			n, err := r.bits(15)
			if err != nil {
				return nil, err
			}
			raw, err := r.bytes(n)
			if err != nil {
				return nil, err
			}
			out = append(out, raw...)
			z.remember(raw)
			continue
		}
		count, err := r.matchLength()
		if err != nil {
			return nil, err
		}
		if len(out)-start+count > zgfxMaxOutput {
			return nil, fmt.Errorf("%w: expands past %d bytes", ErrZGFX, zgfxMaxOutput)
		}
		if out, err = z.copyMatch(out, distance, count); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// copyMatch appends count bytes starting distance back in the history. The
// source may overlap what it writes, which is how a run repeats.
func (z *Decompressor) copyMatch(out []byte, distance, count int) ([]byte, error) {
	filled := z.at
	if z.full {
		filled = len(z.history)
	}
	if distance > filled {
		return nil, fmt.Errorf("%w: match %d back, history holds %d", ErrZGFX, distance, filled)
	}
	src := (z.at - distance + len(z.history)) % len(z.history)
	for range count {
		b := z.history[src]
		out = append(out, b)
		z.history[z.at] = b
		z.at++
		if z.at == len(z.history) {
			z.at, z.full = 0, true
		}
		src++
		if src == len(z.history) {
			src = 0
		}
	}
	return out, nil
}

func (z *Decompressor) remember(p []byte) {
	for len(p) > 0 {
		n := copy(z.history[z.at:], p)
		p = p[n:]
		z.at += n
		if z.at == len(z.history) {
			z.at, z.full = 0, true
		}
	}
}

// bitReader reads most significant bit first.
type bitReader struct {
	data []byte
	pos  int // bit offset
	left int // bits not yet read, excluding padding
}

func (r *bitReader) bits(n int) (int, error) {
	if n > r.left {
		return 0, fmt.Errorf("%w: wanted %d bits, %d remain", ErrZGFX, n, r.left)
	}
	v := 0
	for range n {
		bit := r.data[r.pos>>3] >> (7 - r.pos&7) & 1
		v = v<<1 | int(bit)
		r.pos++
	}
	r.left -= n
	return v, nil
}

func (r *bitReader) token() (zgfxToken, error) {
	code, have := 0, 0
	for _, t := range zgfxTokens {
		for have < t.bits {
			b, err := r.bits(1)
			if err != nil {
				return zgfxToken{}, err
			}
			code = code<<1 | b
			have++
		}
		if code == t.code {
			return t, nil
		}
	}
	return zgfxToken{}, fmt.Errorf("%w: no token for prefix %b", ErrZGFX, code)
}

// matchLength reads a match's length: 3, or a doubling prefix of ones and
// that many extra bits (MS-RDPEGFX 3.1.9.1.3).
func (r *bitReader) matchLength() (int, error) {
	b, err := r.bits(1)
	if err != nil {
		return 0, err
	}
	if b == 0 {
		return 3, nil
	}
	count, extra := 4, 2
	for {
		b, err := r.bits(1)
		if err != nil {
			return 0, err
		}
		if b == 0 {
			break
		}
		count *= 2
		extra++
		if extra > 24 {
			return 0, fmt.Errorf("%w: match length overflows", ErrZGFX)
		}
	}
	v, err := r.bits(extra)
	if err != nil {
		return 0, err
	}
	return count + v, nil
}

// bytes skips to the next byte boundary and takes n whole bytes.
func (r *bitReader) bytes(n int) ([]byte, error) {
	if skip := (8 - r.pos&7) & 7; skip > 0 {
		if _, err := r.bits(skip); err != nil {
			return nil, err
		}
	}
	if n*8 > r.left {
		return nil, fmt.Errorf("%w: %d raw bytes, %d bits remain", ErrZGFX, n, r.left)
	}
	p := r.data[r.pos>>3 : r.pos>>3+n]
	r.pos += 8 * n
	r.left -= 8 * n
	return p, nil
}
