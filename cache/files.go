package cache

import (
	"errors"
	"slices"
	"sync"
	"time"
)

type file struct {
	path         string
	size         uint64
	lastAccessed time.Time
}

type files struct {
	mu    sync.Mutex
	files []file // stored sorted by descending last accessed time
}

func (l *files) InsertOrReplace(f file) (old file, replaced bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	old, replaced = l.delete(f)
	l.insert(f)
	return
}

func (l *files) Delete(f file) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, replaced := l.delete(f)
	return replaced
}

func (l *files) insert(f file) {
	i, _ := slices.BinarySearchFunc(l.files, f, func(a file, b file) int {
		return b.lastAccessed.Compare(a.lastAccessed)
	})
	l.files = slices.Insert(l.files, i, f)
	return
}

func (l *files) delete(f file) (old file, replaced bool) {
	if i := slices.IndexFunc(l.files, func(prev file) bool {
		return prev.path == f.path
	}); i >= 0 {
		replaced = true
		old = l.files[i]
		slices.Delete(l.files, i, i+1)
	}
	return
}

var errRangeDone = errors.New("skip the rest")

// Range goes through files from the oldest access time to the newest
func (l *files) Range(f func(f file) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.files) - 1; i >= 0; i-- {
		err := f(l.files[i])
		//goland:noinspection GoDirectComparisonOfErrors
		if err == errRangeDone {
			return nil
		} else if err != nil {
			return err
		}
	}
	return nil
}
