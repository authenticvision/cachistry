package cache

import (
	"errors"
	"sync"
)

func newFiles() *files {
	return &files{
		byPath: make(map[string]*node),
	}
}

type file struct {
	path string
	size uint64
}

type node struct {
	prev, next *node
	f          file
}

type files struct {
	mu     sync.Mutex
	byPath map[string]*node
	head   *node // newest
	tail   *node // oldest
}

// InsertOrReplace promotes f to the newest file in the cache.
// If f.path was cached, then the old entry is returned and replaced is true.
func (l *files) InsertOrReplace(f file) (old file, replaced bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n, ok := l.byPath[f.path]; ok {
		replaced = true
		old = n.f
		n.f = f
		if n != l.head {
			l.unlink(n)
			l.insertFront(n)
		}
		return
	}
	n := &node{f: f}
	l.byPath[f.path] = n
	l.insertFront(n)
	return
}

func (l *files) Delete(f file) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byPath == nil {
		return false
	}
	n, ok := l.byPath[f.path]
	if !ok {
		return false
	}
	delete(l.byPath, f.path)
	l.unlink(n)
	return true
}

var errRangeDone = errors.New("skip the rest")

// Range goes through files from the oldest access time to the newest
func (l *files) Range(fn func(f file) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for n := l.tail; n != nil; n = n.prev {
		if err := fn(n.f); err != nil {
			//goland:noinspection GoDirectComparisonOfErrors
			if err == errRangeDone {
				return nil
			}
			return err
		}
	}
	return nil
}

func (l *files) insertFront(n *node) {
	n.prev = nil
	n.next = l.head
	if l.head != nil {
		l.head.prev = n
	} else {
		l.tail = n
	}
	l.head = n
}

func (l *files) unlink(n *node) {
	if n.prev != nil {
		n.prev.next = n.next
	} else if l.head == n {
		l.head = n.next
	}
	if n.next != nil {
		n.next.prev = n.prev
	} else if l.tail == n {
		l.tail = n.prev
	}
	n.prev, n.next = nil, nil
}
