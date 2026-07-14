//go:build !linux

package crypt

// On non-Linux platforms (used only for local development on macOS/Windows),
// the buffer is a heap slice without mlock. The runtime still zeroizes it on
// Destroy(). Production targets Linux exclusively.

type lockedBuf struct {
	buf []byte
	n   int
}

func newLockedBuf(cap int) (*lockedBuf, error) {
	return &lockedBuf{buf: make([]byte, cap), n: 0}, nil
}

func (l *lockedBuf) Bytes() []byte {
	if l == nil || l.buf == nil {
		return nil
	}
	if l.n == 0 {
		return l.buf
	}
	return l.buf[:l.n]
}

func (l *lockedBuf) Len() int {
	if l == nil {
		return 0
	}
	return l.n
}

func (l *lockedBuf) Destroy() {
	if l == nil || l.buf == nil {
		return
	}
	for i := range l.buf {
		l.buf[i] = 0
	}
	l.buf = nil
	l.n = 0
}
