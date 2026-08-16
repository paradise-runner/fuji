// Package mutqueue implements the file mutation queue (core-spec §4.5,
// corresponding to the file-mutation-queue module): all file-mutating tools
// serialize through a per-session queue so operations on the same file never
// interleave. Sequential execution makes this trivially safe today; the queue
// is kept for contract parity and future parallel mode.
package mutqueue

import (
	"sync"
)

// Queue serializes mutations per file path.
type Queue struct {
	mu    sync.Mutex
	locks map[string]*fileLock
}

type fileLock struct {
	mu   sync.Mutex
	refs int
}

// New returns an empty queue.
func New() *Queue {
	return &Queue{locks: map[string]*fileLock{}}
}

// WithLock runs fn while holding the lock for path.
func (q *Queue) WithLock(path string, fn func() error) error {
	fl := q.acquire(path)
	defer q.release(path, fl)
	return fn()
}

func (q *Queue) acquire(path string) *fileLock {
	q.mu.Lock()
	fl := q.locks[path]
	if fl == nil {
		fl = &fileLock{}
		q.locks[path] = fl
	}
	fl.refs++
	q.mu.Unlock()
	fl.mu.Lock()
	return fl
}

func (q *Queue) release(path string, fl *fileLock) {
	fl.mu.Unlock()
	q.mu.Lock()
	fl.refs--
	if fl.refs == 0 {
		delete(q.locks, path)
	}
	q.mu.Unlock()
}
