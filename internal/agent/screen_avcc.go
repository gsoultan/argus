package agent

import (
	"errors"
)

// avccToAnnexB turns length-prefixed NAL units (AVCC, as VideoToolbox
// writes them, lengths of size bytes) into Annex B, the start-coded stream
// the rest of the screen path reads, appended to dst. header, when given,
// goes first: the SPS and PPS, already in Annex B, for a key frame.
func avccToAnnexB(dst, header, avcc []byte, size int) ([]byte, error) {
	if size < 1 || size > 4 {
		return nil, errors.New("a NAL length of an impossible size")
	}
	dst = append(dst, header...)
	for len(avcc) > 0 {
		if len(avcc) < size {
			return nil, errors.New("a truncated NAL length")
		}
		var n int
		for _, b := range avcc[:size] {
			n = n<<8 | int(b)
		}
		avcc = avcc[size:]
		if n > len(avcc) {
			return nil, errors.New("a NAL unit longer than the picture")
		}
		dst = append(dst, 0, 0, 0, 1)
		dst = append(dst, avcc[:n]...)
		avcc = avcc[n:]
	}
	return dst, nil
}

// annexBNAL is one parameter set, start-coded.
func annexBNAL(dst, nal []byte) []byte {
	return append(append(dst, 0, 0, 0, 1), nal...)
}
