package cache

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

type Cache struct {
	mu       sync.Mutex
	root     *os.Root
	usedSize uint64
	maxSize  uint64
}

func NewCache(path string, maxSize uint64) (*Cache, error) {
	r, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("open storage dir: %w", err)
	}
	return &Cache{
		root:    r,
		maxSize: maxSize,
	}, nil
}

const cacheMIMEXAttr = "user.com.authenticvision.docker-registry-caching-proxy.mimetype"

func (c *Cache) GetMIMEType(path string) (string, error) {
	_, err := c.root.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	sanitizedPath := filepath.Join(c.root.Name(), filepath.Join("/", path))
	mimeType := make([]byte, 256)
	n, err := unix.Getxattr(sanitizedPath, cacheMIMEXAttr, mimeType)
	if err != nil {
		return "", fmt.Errorf("getxattr: %w", err)
	}
	return string(mimeType[:n]), nil
}

func (c *Cache) FS() fs.FS {
	return c.root.FS()
}

func (c *Cache) Store(path string, mimeType string) (io.WriteCloser, error) {
	err := c.root.MkdirAll(filepath.Dir(path), fs.ModePerm)
	if err != nil {
		return nil, err
	}
	f, err := c.root.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0666)
	if err != nil {
		return nil, err
	}
	err = unix.Setxattr(f.Name(), cacheMIMEXAttr, []byte(mimeType), 0)
	if err != nil {
		return nil, fmt.Errorf("setxattr: %w", err)
	}
	return f, nil
}
