// Package bufio implements buffered I/O. It wraps an io.Reader or io.Writer
// object, creating another object (Reader or Writer) that also implements the
// interface but provides buffering and some help for textual I/O.
package bufiog

import (
	"errors"
	"io"
)

var errNegativeRead = errors.New("bufiog: reader returned negative count from Read")

var (
	ErrInvalidUnreadElem = errors.New("bufiog: invalid use of UnreadElem")
	ErrBufferFull        = errors.New("bufiog: buffer full")
	ErrNegativeCount     = errors.New("bufiog: negative count")
)

const (
	defaultBufSize           = 64
	maxConsecutiveEmptyReads = 100
	minReadBufferSize        = 16
)

type ReadInterface[T any] interface {
	Read(p []T) (int, error)
}

type ReadFromInterface[T any] interface {
	ReadFrom(r ReadInterface[T]) (n int64, err error)
}

// Reader implements buffering for an io.Reader object.
type Reader[T any] struct {
	buf      []T
	rd       ReadInterface[T] // reader provided by the client
	r, w     int              // buf read and write positions
	err      error
	lastElem *T // last element read for UnreadElem; nil means invalid
}

// NewReader returns a new Reader whose buffer has the default size.
func NewReader[T any](rd ReadInterface[T]) *Reader[T] {
	return NewReaderSize(rd, defaultBufSize)
}

// NewReaderSize returns a new Reader whose buffer has at least the specified
// size. If the argument io.Reader is already a Reader with large enough
// size, it returns the underlying Reader.
func NewReaderSize[T any](rd ReadInterface[T], size int) *Reader[T] {
	// Is it already a Reader?
	b, ok := rd.(*Reader[T])
	if ok && len(b.buf) >= size {
		return b
	}
	if size < minReadBufferSize {
		size = minReadBufferSize
	}
	r := new(Reader[T])
	r.reset(make([]T, size), rd)
	return r
}

// Buffered returns the number of elements that can be read from the current buffer.
func (b *Reader[T]) Buffered() int { return b.w - b.r }

// Discard skips the next n elements, returning the number of elements discarded.
//
// If Discard skips fewer than n elements, it also returns an error.
// If 0 <= n <= b.Buffered(), Discard is guaranteed to succeed without
// reading from the underlying io.Reader.
func (b *Reader[T]) Discard(n int) (discarded int, err error) {
	if n < 0 {
		return 0, ErrNegativeCount
	}
	if n == 0 {
		return
	}

	b.lastElem = nil

	remain := n
	for {
		skip := b.Buffered()
		if skip == 0 {
			b.fill()
			skip = b.Buffered()
		}
		if skip > remain {
			skip = remain
		}
		b.r += skip
		remain -= skip
		if remain == 0 {
			return n, nil
		}
		if b.err != nil {
			return n - remain, b.readErr()
		}
	}
}

// Peek returns the next n elements without advancing the reader. The elements stop
// being valid at the next read call. If Peek returns fewer than n elements, it
// also returns an error explaining why the read is short. The error is
// ErrBufferFull if n is larger than b's buffer size.
//
// Calling Peek prevents a UnreadElem call from succeeding
// until the next read operation.
func (b *Reader[T]) Peek(n int) ([]T, error) {
	if n < 0 {
		return nil, ErrNegativeCount
	}

	b.lastElem = nil

	for b.w-b.r < n && b.w-b.r < len(b.buf) && b.err == nil {
		b.fill() // b.w-b.r < len(b.buf) => buffer is not full
	}

	if n > len(b.buf) {
		return b.buf[b.r:b.w], ErrBufferFull
	}

	// 0 <= n <= len(b.buf)
	var err error
	if avail := b.w - b.r; avail < n {
		// not enough data in buffer
		n = avail
		err = b.readErr()
		if err == nil {
			err = ErrBufferFull
		}
	}
	return b.buf[b.r : b.r+n], err
}

// Read reads data into p.
// It returns the number of elements read into p.
// The elements are taken from at most one Read on the underlying Reader,
// hence n may be less than len(p).
// To read exactly len(p) elements, use io.ReadFull(b, p).
// If the underlying Reader can return a non-zero count with io.EOF,
// then this Read method can do so as well; see the [io.Reader] docs.
func (b *Reader[T]) Read(p []T) (n int, err error) {
	n = len(p)
	if n == 0 {
		if b.Buffered() > 0 {
			return 0, nil
		}
		return 0, b.readErr()
	}
	if b.r == b.w {
		if b.err != nil {
			return 0, b.readErr()
		}
		if len(p) >= len(b.buf) {
			// Large read, empty buffer.
			// Read directly into p to avoid copy.
			n, b.err = b.rd.Read(p)
			if n < 0 {
				panic(errNegativeRead)
			}
			if n > 0 {
				b.lastElem = &p[n-1]
			}
			return n, b.readErr()
		}
		// One read.
		// Do not use b.fill, which will loop.
		b.r = 0
		b.w = 0
		n, b.err = b.rd.Read(b.buf)
		if n < 0 {
			panic(errNegativeRead)
		}
		if n == 0 {
			return 0, b.readErr()
		}
		b.w += n
	}

	// copy as much as we can
	// Note: if the slice panics here, it is probably because
	// the underlying reader returned a bad count. See issue 49795.
	n = copy(p, b.buf[b.r:b.w])
	b.r += n
	b.lastElem = &b.buf[b.r-1]
	return n, nil
}

// ReadElem reads and returns a single element.
// If no element is available, returns an error.
func (b *Reader[T]) ReadElem() (T, error) {
	for b.r == b.w {
		if b.err != nil {
			var zero T
			return zero, b.readErr()
		}
		b.fill() // buffer is empty
	}
	c := b.buf[b.r]
	b.r++
	b.lastElem = &c
	return c, nil
}

// Reset discards any buffered data, resets all state, and switches
// the buffered reader to read from r.
// Calling Reset on the zero value of Reader initializes the internal buffer
// to the default size.
// Calling b.Reset(b) (that is, resetting a Reader to itself) does nothing.
func (b *Reader[T]) Reset(r ReadInterface[T]) {
	// If a Reader r is passed to NewReader, NewReader will return r.
	// Different layers of code may do that, and then later pass r
	// to Reset. Avoid infinite recursion in that case.
	if b == r {
		return
	}
	if b.buf == nil {
		b.buf = make([]T, defaultBufSize)
	}
	b.reset(b.buf, r)
}

// Size returns the length of the underlying buffer in elements.
func (b *Reader[T]) Size() int { return len(b.buf) }

func (b *Reader[T]) UnreadElem() error {
	if b.lastElem == nil || b.r == 0 && b.w > 0 {
		return ErrInvalidUnreadElem
	}
	// b.r > 0 || b.w == 0
	if b.r > 0 {
		b.r--
	} else {
		// b.r == 0 && b.w == 0
		b.w = 1
	}
	b.buf[b.r] = *b.lastElem
	b.lastElem = nil
	return nil
}

// WriteTo implements io.WriterTo.
// This may make multiple calls to the Read method of the underlying Reader.
// If the underlying reader supports the WriteTo method,
// this calls the underlying WriteTo without buffering.
func (b *Reader[T]) WriteTo(w WriteInterface[T]) (n int64, err error) {
	b.lastElem = nil

	n, err = b.writeBuf(w)
	if err != nil {
		return
	}

	if r, ok := b.rd.(WriteToInterface[T]); ok {
		m, err := r.WriteTo(w)
		n += m
		return n, err
	}

	if w, ok := w.(ReadFromInterface[T]); ok {
		m, err := w.ReadFrom(b.rd)
		n += m
		return n, err
	}

	if b.w-b.r < len(b.buf) {
		b.fill() // buffer not full
	}

	for b.r < b.w {
		// b.r < b.w => buffer is not empty
		m, err := b.writeBuf(w)
		n += m
		if err != nil {
			return n, err
		}
		b.fill() // buffer is empty
	}

	if b.err == io.EOF {
		b.err = nil
	}

	return n, b.readErr()
}

// fill reads a new chunk into the buffer.
func (b *Reader[T]) fill() {
	// Slide existing data to beginning.
	if b.r > 0 {
		copy(b.buf, b.buf[b.r:b.w])
		b.w -= b.r
		b.r = 0
	}

	if b.w >= len(b.buf) {
		panic("bufio: tried to fill full buffer")
	}

	// Read new data: try a limited number of times.
	for i := maxConsecutiveEmptyReads; i > 0; i-- {
		n, err := b.rd.Read(b.buf[b.w:])
		if n < 0 {
			panic(errNegativeRead)
		}
		b.w += n
		if err != nil {
			b.err = err
			return
		}
		if n > 0 {
			return
		}
	}
	b.err = io.ErrNoProgress
}
func (b *Reader[T]) readErr() error {
	err := b.err
	b.err = nil
	return err
}
func (b *Reader[T]) reset(buf []T, r ReadInterface[T]) {
	*b = Reader[T]{
		buf: buf,
		rd:  r,
	}
}

// writeBuf writes the Reader's buffer to the writer.
func (b *Reader[T]) writeBuf(w WriteInterface[T]) (int64, error) {
	n, err := w.Write(b.buf[b.r:b.w])
	if n < 0 {
		panic(errNegativeWrite)
	}
	b.r += n
	return int64(n), err
}
