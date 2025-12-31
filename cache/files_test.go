package cache

import (
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFiles_HappyPath(t *testing.T) {
	r := require.New(t)
	l := newFiles()

	mustRange := func() []file {
		var got []file
		r.NoError(l.Range(func(f file) error {
			got = append(got, f)
			return nil
		}))
		return got
	}

	paths := func(fs []file) []string {
		out := make([]string, 0, len(fs))
		for _, f := range fs {
			out = append(out, f.path)
		}
		return out
	}

	find := func(fs []file, path string) (file, bool) {
		for _, f := range fs {
			if f.path == path {
				return f, true
			}
		}
		return file{}, false
	}

	a := file{path: "a", size: 1}
	b := file{path: "b", size: 2}
	c := file{path: "c", size: 3}

	old, replaced := l.InsertOrReplace(a)
	r.False(replaced)
	r.Equal(file{}, old)

	_, replaced = l.InsertOrReplace(b)
	r.False(replaced)
	_, replaced = l.InsertOrReplace(c)
	r.False(replaced)

	got := mustRange()
	r.Equal([]string{"a", "b", "c"}, paths(got))

	// refresh (insert again) promotes to newest and replaces data
	b2 := file{path: "b", size: 20}
	old, replaced = l.InsertOrReplace(b2)
	r.True(replaced)
	r.Equal(b, old)

	got = mustRange()
	r.Equal([]string{"a", "c", "b"}, paths(got))
	fb, ok := find(got, "b")
	r.True(ok)
	r.Equal(uint64(20), fb.size)

	// range early stop via errRangeDone
	var partial []string
	err := l.Range(func(f file) error {
		partial = append(partial, f.path)
		if len(partial) == 2 {
			return errRangeDone
		}
		return nil
	})
	r.NoError(err)
	r.Equal([]string{"a", "c"}, partial)

	// delete existing and missing
	r.True(l.Delete(file{path: "c"}))
	r.False(l.Delete(file{path: "missing"}))

	got = mustRange()
	r.Equal([]string{"a", "b"}, paths(got))

	// delete head (newest) then tail (oldest)
	r.True(l.Delete(file{path: "b"}))
	got = mustRange()
	r.Equal([]string{"a"}, paths(got))

	r.True(l.Delete(file{path: "a"}))
	got = mustRange()
	r.Empty(got)

	// cache still functional after becoming empty
	_, replaced = l.InsertOrReplace(file{path: "x", size: 9})
	r.False(replaced)
	got = mustRange()
	r.Equal([]string{"x"}, paths(got))
}

func BenchmarkFiles_InsertDelete(b *testing.B) {
	// pre-populate cache to simulate os.WalkDir result
	const cacheSize = 1000
	items := make([]file, cacheSize)
	for i := 0; i < cacheSize; i++ {
		items[i] = file{path: fmt.Sprintf("file-%d", i)}
	}
	l := newFiles()
	for _, i := range rand.Perm(cacheSize) {
		l.InsertOrReplace(items[i])
	}

	b.ResetTimer()
	for range b.N + 1 {
		l.Delete(items[rand.IntN(cacheSize)])
		l.InsertOrReplace(items[rand.IntN(cacheSize)])
	}
}
