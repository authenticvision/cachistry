package cache

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type Cache struct {
	root *os.Root

	mu       sync.Mutex
	usedSize uint64
	maxSize  uint64
}

const tmpDir = "-/tmp"

func NewCache(path string, maxSize uint64) (*Cache, error) {
	r, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("openroot: %w", err)
	}
	err = r.MkdirAll(tmpDir, 0777)
	if err != nil {
		return nil, fmt.Errorf("mkdir tmp: %w", err)
	}
	return &Cache{
		root:    r,
		maxSize: maxSize,
	}, nil
}

const xattrMIME = "user.com.authenticvision.docker-registry-caching-proxy.mimetype"
const xattrETag = "user.com.authenticvision.docker-registry-caching-proxy.etag"
const xattrValidated = "user.com.authenticvision.docker-registry-caching-proxy.validated" // timestamp when ETag was last verified (RFC 3339)

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
func (c *Cache) Store(f *os.File, path string) error {
	err := c.root.MkdirAll(filepath.Dir(path), fs.ModePerm)
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
	return nil
}

func (c *Cache) UpdateValidated(path string) error {
	return setXAttr(c.absoluteInRoot(path), xattrValidated, time.Now().UTC().Format(time.RFC3339))
}

func (c *Cache) relativeToRoot(path string) string {
	root := c.root.Name()
	if strings.HasPrefix(path, root) {
		path = path[len(root):]
	}
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
