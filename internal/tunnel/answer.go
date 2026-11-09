package tunnel

import (
	"errors"
	"io"
)

/*
An answer larger than a frame.

A store session's answer (a RabbitMQ queue with its consumers, a page of
MongoDB documents, a Redis value) may carry up to MaxStoreAnswer bytes of
values, far more than MaxFrame. Written as one frame it failed, and the
failure ended the session: the person saw "the connection to the agent ended"
for asking to look at something.

So an answer that does not fit is cut into frames: every piece but the last
is a FramePGAnswerPart, and the last is the FramePGAnswer it always was. A
reader that knows parts (AnswerJoiner) puts them back together; one that
does not ignores the parts, as it ignores any frame type it does not know,
and so a grant only says parts may be sent when the control plane signing it
joins them (GrantDB.AnswerParts).
*/

// MaxAnswer bounds one answer as it crosses the tunnel, all its parts
// together: MaxStoreAnswer of values with room for the JSON around them.
const MaxAnswer = MaxStoreAnswer + MaxStoreAnswer/2

// ErrAnswerTooLarge is an answer past MaxAnswer, or past one frame where
// parts may not be sent.
var ErrAnswerTooLarge = errors.New("the answer is too large to send; ask for fewer")

// WriteAnswer writes one encoded answer, in parts when parts is set and it
// does not fit a frame. The caller holds the stream's writer for the whole
// call, so another frame cannot land between two parts.
func WriteAnswer(w io.Writer, b []byte, parts bool) error {
	if len(b) <= MaxFrame {
		return WriteFrame(w, FramePGAnswer, b)
	}
	if !parts || len(b) > MaxAnswer {
		return ErrAnswerTooLarge
	}
	for len(b) > MaxFrame {
		if err := WriteFrame(w, FramePGAnswerPart, b[:MaxFrame]); err != nil {
			return err
		}
		b = b[MaxFrame:]
	}
	return WriteFrame(w, FramePGAnswer, b)
}

// AnswerJoiner puts an answer sent in parts back together.
type AnswerJoiner struct {
	buf []byte
}

// Part keeps one FramePGAnswerPart. It fails once the parts pass MaxAnswer,
// which only a broken or hostile agent sends.
func (j *AnswerJoiner) Part(p []byte) error {
	if len(j.buf)+len(p) > MaxAnswer {
		j.buf = nil
		return ErrAnswerTooLarge
	}
	j.buf = append(j.buf, p...)
	return nil
}

// Answer is the whole answer that FramePGAnswer p ends. With no parts
// before it, it is p itself, valid until the reader's next frame.
func (j *AnswerJoiner) Answer(p []byte) ([]byte, error) {
	if len(j.buf) == 0 {
		return p, nil
	}
	if len(j.buf)+len(p) > MaxAnswer {
		j.buf = nil
		return nil, ErrAnswerTooLarge
	}
	whole := append(j.buf, p...)
	// Not kept for the next answer: most are small, and one large answer
	// should not hold its buffer for the rest of the session.
	j.buf = nil
	return whole, nil
}
