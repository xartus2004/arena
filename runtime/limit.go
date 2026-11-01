package runtime

import "sync"

// LimitedBuffer is an io.Writer that keeps at most Max bytes and silently
// discards the rest, remembering that it did. It never returns an error, so
// a chatty process cannot stall or crash the reader. It is safe for
// concurrent use.
type LimitedBuffer struct {
	Max int

	mu        sync.Mutex
	buf       []byte
	truncated bool
	total     int64
}

// NewLimitedBuffer returns a buffer that keeps at most max bytes
// (DefaultMaxOutput if max <= 0).
func NewLimitedBuffer(max int) *LimitedBuffer {
	if max <= 0 {
		max = DefaultMaxOutput
	}
	return &LimitedBuffer{Max: max}
}

// Write implements io.Writer.
func (b *LimitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.total += int64(len(p))
	room := b.Max - len(b.buf)
	if room <= 0 {
		b.truncated = b.truncated || len(p) > 0
		return len(p), nil
	}
	if len(p) > room {
		b.buf = append(b.buf, p[:room]...)
		b.truncated = true
		return len(p), nil
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

// Bytes returns a copy of the retained bytes.
func (b *LimitedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf...)
}

// Truncated reports whether any bytes were discarded.
func (b *LimitedBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

// Total reports how many bytes were written in total.
func (b *LimitedBuffer) Total() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}
