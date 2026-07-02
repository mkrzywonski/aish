package term

import "sync"

// Ring is a fixed-capacity byte ring with a monotonic absolute offset, so
// readers can poll incrementally with a cursor and detect data they missed.
type Ring struct {
	mu   sync.Mutex
	data []byte
	end  int64 // absolute offset of the next byte to be written
}

func NewRing(size int) *Ring {
	return &Ring{data: make([]byte, size)}
}

func (r *Ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w := p
	for len(w) > 0 {
		off := int(r.end % int64(len(r.data)))
		n := copy(r.data[off:], w)
		w = w[n:]
		r.end += int64(n)
	}
	return len(p), nil
}

// End returns the absolute offset just past the newest byte.
func (r *Ring) End() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.end
}

// ReadFrom returns up to max bytes starting at the absolute offset cursor.
// If cursor has aged out of the ring, reading starts at the oldest retained
// byte and dropped reports how many bytes were missed. A negative cursor
// means "the last max bytes".
func (r *Ring) ReadFrom(cursor int64, max int) (data []byte, next int64, dropped int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	oldest := r.end - int64(len(r.data))
	if oldest < 0 {
		oldest = 0
	}
	if cursor < 0 {
		cursor = r.end - int64(max)
		if cursor < oldest {
			cursor = oldest
		}
	} else if cursor < oldest {
		dropped = oldest - cursor
		cursor = oldest
	}
	if cursor > r.end {
		cursor = r.end
	}

	n := r.end - cursor
	if n > int64(max) {
		n = int64(max)
	}
	out := make([]byte, n)
	for i := int64(0); i < n; {
		off := int((cursor + i) % int64(len(r.data)))
		c := copy(out[i:], r.data[off:])
		i += int64(c)
	}
	return out, cursor + n, dropped
}
