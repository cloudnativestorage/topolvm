// Package lockutil provides an exact-match, per-key mutex.
package lockutil

import (
	"fmt"
	"sync"
)

// KeyMutex is an exact-match per-key mutex: distinct keys never contend with
// each other, unlike k8s.io/utils/keymutex's hashed implementation where
// unrelated keys can collide onto the same bucket and block one another.
// It implements the same LockKey/UnlockKey shape as keymutex.KeyMutex.
type KeyMutex struct {
	mu      sync.Mutex
	entries map[string]*entry
}

type entry struct {
	mu       sync.Mutex
	refCount int
}

// New returns a ready-to-use KeyMutex.
func New() *KeyMutex {
	return &KeyMutex{entries: make(map[string]*entry)}
}

// LockKey acquires the lock associated with id, creating it if needed.
func (k *KeyMutex) LockKey(id string) {
	k.mu.Lock()
	e, ok := k.entries[id]
	if !ok {
		e = &entry{}
		k.entries[id] = e
	}
	e.refCount++
	k.mu.Unlock()

	e.mu.Lock()
}

// UnlockKey releases the lock associated with id. The entry is removed once
// no goroutine holds or is waiting on it, so memory use tracks the number of
// currently-locked keys rather than the number of keys ever seen.
func (k *KeyMutex) UnlockKey(id string) error {
	k.mu.Lock()
	e, ok := k.entries[id]
	if !ok {
		k.mu.Unlock()
		return fmt.Errorf("lockutil: key %q is not locked", id)
	}
	e.refCount--
	if e.refCount == 0 {
		delete(k.entries, id)
	}
	k.mu.Unlock()

	e.mu.Unlock()
	return nil
}
