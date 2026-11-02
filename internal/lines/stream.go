// Package lines turns Docker's log bytes into records. Everything here reads
// text that outsiders can influence, so nothing in it may panic, and a line it
// can't make sense of is stored as plain text rather than dropped.
package lines

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"time"
)

// Streams, as stored.
const (
	Stdout int16 = 1
	Stderr int16 = 2
)

const (
	frameHeader = 8

	// maxLine is the longest line kept; the rest is cut and marked.
	maxLine = 32 << 10

	// maxFrame is the largest frame read; anything longer isn't Docker's framing.
	maxFrame = 1 << 20
)

// Raw is one line as Docker delivered it.
type Raw struct {
	Time   time.Time // zero when Docker's timestamp couldn't be read
	Stream int16
	Text   []byte
}

// Reader reads lines from a log stream.
type Reader struct {
	r   *bufio.Reader
	tty bool
	buf []byte
}

// NewReader reads a stream from Docker, requested with timestamps. A container
// with a TTY has one combined output with no framing.
func NewReader(r io.Reader, tty bool) *Reader {
	return &Reader{r: bufio.NewReaderSize(r, 64<<10), tty: tty}
}

// Next returns the next line. io.EOF means the stream ended cleanly.
func (r *Reader) Next() (Raw, error) {
	if r.tty {
		return r.nextTTY()
	}
	return r.nextFrame()
}

// nextFrame reads one Docker frame: one message, so one line.
func (r *Reader) nextFrame() (Raw, error) {
	var head [frameHeader]byte
	if _, err := io.ReadFull(r.r, head[:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return Raw{}, io.EOF
		}
		return Raw{}, err
	}
	size := binary.BigEndian.Uint32(head[4:])
	stream := Stdout
	if head[0] == 2 {
		stream = Stderr
	}
	if size > maxFrame {
		return Raw{}, errors.New("Docker: a log frame claims to be over 1 MiB: the stream isn't framed as expected")
	}
	if cap(r.buf) < int(size) {
		r.buf = make([]byte, size)
	}
	r.buf = r.buf[:size]
	if _, err := io.ReadFull(r.r, r.buf); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return Raw{}, io.EOF
		}
		return Raw{}, err
	}
	return split(r.buf, stream), nil
}

// nextTTY reads one newline-ended line of a stream without framing.
func (r *Reader) nextTTY() (Raw, error) {
	line, err := r.r.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		long := append([]byte(nil), line...)
		for errors.Is(err, bufio.ErrBufferFull) {
			line, err = r.r.ReadSlice('\n')
			if len(long) < maxLine*2 {
				long = append(long, line...)
			}
		}
		line = long
	}
	if len(line) == 0 && err != nil {
		return Raw{}, err
	}
	return split(line, Stdout), nil
}

// split separates Docker's timestamp from the line. The text is a copy.
func split(line []byte, stream int16) Raw {
	line = bytes.TrimRight(line, "\r\n")
	out := Raw{Stream: stream}
	if word, rest, ok := bytes.Cut(line, []byte(" ")); ok {
		if t, err := time.Parse(time.RFC3339Nano, string(word)); err == nil {
			out.Time, line = t.UTC(), rest
		}
	} else if t, err := time.Parse(time.RFC3339Nano, string(line)); err == nil {
		out.Time, line = t.UTC(), nil
	}
	out.Text = append([]byte{}, line...)
	return out
}
