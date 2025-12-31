package cache

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/authenticvision/util-go/fmtutil"
	"golang.org/x/sys/unix"
)

type Cache struct {
	root *os.Root

	files     *files
	usedBytes uint64
	maxBytes  uint64
}

const tmpDir = "-/tmp"

func NewCache(path string, maxSizeBytes uint64) (*Cache, error) {
	r, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("openroot: %w", err)
	}
	c := &Cache{
		root:     r,
		files:    newFiles(),
		maxBytes: maxSizeBytes,
	}
	err = c.root.MkdirAll(tmpDir, 0777)
	if err != nil {
		return nil, fmt.Errorf("mkdir tmp: %w", err)
	}

	type fileAccessTime struct {
		file
		lastAccessed time.Time
	}
	var discoveredFiles []fileAccessTime

	timeStart := time.Now()
	err = fs.WalkDir(c.root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.HasPrefix(path, tmpDir+"/") {
			err := c.root.Remove(path)
			if err != nil {
				return err
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		discoveredFiles = append(discoveredFiles, fileAccessTime{
			file:         file{path: path, size: uint64(info.Size())},
			lastAccessed: atime(info),
		})
		atomic.AddUint64(&c.usedBytes, uint64(info.Size()))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk storage dir: %w", err)
	}
	timeWalk := time.Now()

	slices.SortFunc(discoveredFiles, func(a, b fileAccessTime) int {
		return int(a.lastAccessed.Sub(b.lastAccessed) / time.Second)
	})
	timeSort := time.Now()

	for _, f := range discoveredFiles {
		c.files.InsertOrReplace(f.file)
	}
	timeInsert := time.Now()

	slog.Info(
		"cache initialized",
		slog.String("path", path),
		slog.Duration("perf_walk", timeWalk.Sub(timeStart)),
		slog.Duration("perf_sort", timeSort.Sub(timeWalk)),
		slog.Duration("perf_insert", timeInsert.Sub(timeSort)),
		c.statAttr(),
	)
	return c, nil
}

func (c *Cache) statAttr() slog.Attr {
	used := atomic.LoadUint64(&c.usedBytes)
	return slog.GroupAttrs("stats",
		slog.Float64("used_percent", 100*float64(used)/float64(c.maxBytes)),
		slog.String("used", fmtutil.FormatBytes(used)),
		slog.String("max", fmtutil.FormatBytes(c.maxBytes)),
	)
}

const xattrMIME = "user.com.authenticvision.cachistry.mimetype"
const xattrETag = "user.com.authenticvision.cachistry.etag"
const xattrValidated = "user.com.authenticvision.cachistry.validated" // timestamp when ETag was last verified (RFC 3339)

type Cached struct {
	MIMEType  string
	ETag      string
	Validated time.Time
}

// Get checks if path is in cache and if so, updates its atime and returns its
// mime type, ETag and last validation time.
func (c *Cache) Get(path string) (*Cached, error) {
	err := c.root.Chtimes(path, time.Now(), time.Time{})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	mimeType, err := getXAttr(c.absoluteInRoot(path), xattrMIME)
	if err != nil {
		return nil, err
	}
	validatedStr, err := getXAttr(c.absoluteInRoot(path), xattrValidated)
	if err != nil {
		return nil, err
	}
	validated, err := time.Parse(time.RFC3339, validatedStr)
	if err != nil {
		return nil, err
	}
	eTag, err := getXAttr(c.absoluteInRoot(path), xattrETag)
	if err != nil {
		return nil, err
	}
	return &Cached{
		MIMEType:  mimeType,
		ETag:      eTag,
		Validated: validated,
	}, nil
}

func (c *Cache) FS() fs.FS {
	return c.root.FS()
}

type TempRemover func()

func (c *Cache) Create(mimeType string, eTag string) (*os.File, TempRemover, error) {
	path := fmt.Sprintf("%s/%d", tmpDir, rand.Uint64())
	f, err := c.root.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0666)
	if err != nil {
		return nil, nil, err
	}
	tempRemover := func() {
		_ = f.Close()
		_ = c.root.Remove(path)
	}
	if err = setXAttr(f.Name(), xattrMIME, mimeType); err != nil {
		return nil, tempRemover, err
	}
	if err = setXAttr(f.Name(), xattrETag, eTag); err != nil {
		return nil, tempRemover, err
	}
	if err := c.UpdateValidated(c.relativeToRoot(f.Name())); err != nil {
		return nil, tempRemover, err
	}
	return f, tempRemover, nil
}

// Store moves a temporary file into place, overriding previously existing files
func (c *Cache) Store(f *os.File, path string, size uint64) error {
	err := c.evict(size)
	if err != nil {
		return fmt.Errorf("evict: %w", err)
	}
	err = c.root.MkdirAll(filepath.Dir(path), fs.ModePerm)
	if err != nil {
		return err
	}
	err = f.Close()
	if err != nil {
		return err
	}
	err = c.root.Rename(c.relativeToRoot(f.Name()), path)
	if err != nil {
		return err
	}
	if old, replaced := c.files.InsertOrReplace(file{
		path: path,
		size: size,
	}); replaced {
		atomicSubtract(&c.usedBytes, old.size)
	}
	atomic.AddUint64(&c.usedBytes, size)
	return nil
}

func atomicSubtract(addr *uint64, delta uint64) uint64 {
	return atomic.AddUint64(addr, ^(delta - 1))
}

func (c *Cache) UpdateValidated(path string) error {
	return setXAttr(c.absoluteInRoot(path), xattrValidated, time.Now().UTC().Format(time.RFC3339))
}

func (c *Cache) evict(size uint64) error {
	if atomic.LoadUint64(&c.usedBytes)+size <= c.maxBytes {
		return nil
	}
	toEvict := int64(size)
	before := c.statAttr()
	err := c.files.Range(func(f file) error {
		slog.Debug("evicting file",
			slog.String("path", f.path),
			slog.String("size", fmtutil.FormatBytes(f.size)),
		)
		err := c.root.Remove(f.path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		atomicSubtract(&c.usedBytes, f.size)
		toEvict -= int64(f.size)
		if toEvict <= 0 {
			return errRangeDone
		}
		return nil
	})
	if err != nil {
		return err
	}
	slog.Debug("evicted files from cache",
		slog.Any("before", before),
		slog.Any("after", c.statAttr()),
		slog.String("deleted", fmtutil.FormatBytes(uint64(int64(size)-toEvict))),
	)
	return nil
}

func (c *Cache) relativeToRoot(path string) string {
	path = strings.TrimPrefix(path, c.root.Name())
	if len(path) > 0 && path[0] == '/' {
		path = path[1:]
	}
	return path
}

func (c *Cache) absoluteInRoot(path string) string {
	sanitized := filepath.Join("/", path)
	return filepath.Join(c.root.Name(), sanitized)
}

func getXAttr(path string, attr string) (string, error) {
	out := make([]byte, 256)
	n, err := unix.Getxattr(path, attr, out)
	if err != nil {
		return "", fmt.Errorf("getxattr %q: %w", attr, err)
	}
	return string(out[:n]), nil
}

func setXAttr(path string, attr string, data string) error {
	err := unix.Setxattr(path, attr, []byte(data), 0)
	if err != nil {
		return fmt.Errorf("setxattr %q: %w", attr, err)
	}
	return nil
}
