//go:build linux

package crypt

import (
	"fmt"

	"golang.org/x/sys/unix"
)

type lockedBuf struct {
	buf    []byte
	n      int
	locked bool
}

func newLockedBuf(cap int) (*lockedBuf, error) {
	// Allocate via mmap so we can mlock a known page-aligned region.
	buf, err := unix.Mmap(-1, 0, cap, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANON)
	if err != nil {
		return nil, fmt.Errorf("crypt: mmap secret buffer: %w", err)
	}
	if err := unix.Mlock(buf); err != nil {
		// Mlock may fail under RLIMIT_MEMLOCK; we keep the buffer usable
		// (with a warning at the call site) rather than aborting.
		return &lockedBuf{buf: buf, n: 0, locked: false}, nil
	}
	return &lockedBuf{buf: buf, n: 0, locked: true}, nil
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
	if l.locked {
		_ = unix.Munlock(l.buf)
	}
	_ = unix.Munmap(l.buf)
	l.buf = nil
	l.n = 0
}
