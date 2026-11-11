package cache

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// LayerCache is a persistent, content-addressed cache for OCI image layers.
// Layers are stored by their SHA256 digest and verified before reuse.
type LayerCache struct {
	dir       string
	mu        sync.Mutex
	downloads map[string]*downloadTracker
}

// downloadTracker tracks an in-flight download to avoid duplicate downloads.
type downloadTracker struct {
	done chan struct{}
	err  error
}

// New creates a LayerCache at the given directory.
func New(dir string) *LayerCache {
	return &LayerCache{
		dir:       dir,
		downloads: make(map[string]*downloadTracker),
	}
}

// EnsureDir creates the cache directory structure.
func (c *LayerCache) EnsureDir() error {
	if err := os.MkdirAll(c.dir, 0o750); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(c.dir, "blobs"), 0o750); err != nil {
		return fmt.Errorf("create blobs dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(c.dir, "tmp"), 0o750); err != nil {
		return fmt.Errorf("create tmp dir: %w", err)
	}
	return nil
}

// Path returns the filesystem path for a cached layer digest.
func (c *LayerCache) Path(digest string) string {
	return filepath.Join(c.dir, "blobs", digest)
}

// Has checks if a layer with the given digest exists in the cache.
func (c *LayerCache) Has(digest string) bool {
	_, err := os.Stat(c.Path(digest))
	return err == nil
}

// Get returns a reader for the cached layer. The caller must close the reader.
func (c *LayerCache) Get(digest string) (io.ReadCloser, error) {
	p := c.Path(digest)
	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("open cached layer %s: %w", digest, err)
	}
	return f, nil
}

// Store stores a layer in the cache. It writes to a temp file then renames
// atomically. If the layer already exists, it is not overwritten.
func (c *LayerCache) Store(digest string, content io.Reader) error {
	c.mu.Lock()
	if c.Has(digest) {
		c.mu.Unlock()
		return nil // already cached
	}

	// Track concurrent downloads to avoid duplicates.
	if tracker, ok := c.downloads[digest]; ok {
		c.mu.Unlock()
		<-tracker.done
		return tracker.err
	}

	tracker := &downloadTracker{done: make(chan struct{})}
	c.downloads[digest] = tracker
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.downloads, digest)
		close(tracker.done)
		c.mu.Unlock()
	}()

	tmpDir := filepath.Join(c.dir, "tmp")
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		tracker.err = fmt.Errorf("create tmp dir: %w", err)
		return tracker.err
	}

	tmpPath := filepath.Join(tmpDir, digest+".tmp")
	out, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		tracker.err = fmt.Errorf("create temp file: %w", err)
		return tracker.err
	}

	h := sha256.New()
	w := io.MultiWriter(out, h)

	if _, err := io.Copy(w, content); err != nil {
		out.Close()
		os.Remove(tmpPath)
		tracker.err = fmt.Errorf("write layer: %w", err)
		return tracker.err
	}

	if err := out.Close(); err != nil {
		os.Remove(tmpPath)
		tracker.err = fmt.Errorf("close temp file: %w", err)
		return tracker.err
	}

	// Verify digest matches expected.
	computed := fmt.Sprintf("sha256:%x", h.Sum(nil))
	if computed != digest {
		os.Remove(tmpPath)
		tracker.err = fmt.Errorf("digest mismatch: expected %s, got %s", digest, computed)
		return tracker.err
	}

	blobsDir := filepath.Join(c.dir, "blobs")
	if err := os.MkdirAll(blobsDir, 0o750); err != nil {
		os.Remove(tmpPath)
		tracker.err = fmt.Errorf("create blobs dir: %w", err)
		return tracker.err
	}

	finalPath := filepath.Join(blobsDir, digest)
	if err := os.Rename(tmpPath, finalPath); err != nil {
		os.Remove(tmpPath)
		tracker.err = fmt.Errorf("rename to cache: %w", err)
		return tracker.err
	}

	return nil
}

// Remove removes a layer from the cache.
func (c *LayerCache) Remove(digest string) error {
	p := c.Path(digest)
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove layer %s: %w", digest, err)
	}
	return nil
}

// List returns all cached layer digests.
func (c *LayerCache) List() ([]string, error) {
	blobsDir := filepath.Join(c.dir, "blobs")
	entries, err := os.ReadDir(blobsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var digests []string
	for _, e := range entries {
		if !e.IsDir() {
			digests = append(digests, e.Name())
		}
	}
	return digests, nil
}

// Size returns the total size of all cached layers in bytes.
func (c *LayerCache) Size() (int64, error) {
	blobsDir := filepath.Join(c.dir, "blobs")
	var total int64
	err := filepath.Walk(blobsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	return total, nil
}

// CleanupTemp removes leftover temp files from interrupted downloads.
func (c *LayerCache) CleanupTemp() error {
	tmpDir := filepath.Join(c.dir, "tmp")
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		p := filepath.Join(tmpDir, e.Name())
		if err := os.Remove(p); err != nil {
			return err
		}
	}
	return nil
}

// Stats returns cache statistics.
type Stats struct {
	Layers    int
	SizeBytes int64
}

// Stats returns cache statistics.
func (c *LayerCache) Stats() (Stats, error) {
	digests, err := c.List()
	if err != nil {
		return Stats{}, err
	}
	size, err := c.Size()
	if err != nil {
		return Stats{}, err
	}
	return Stats{Layers: len(digests), SizeBytes: size}, nil
}

// VerifyIntegrity verifies the SHA256 digest of a cached layer.
func VerifyIntegrity(path, expectedDigest string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open layer: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hash layer: %w", err)
	}
	computed := fmt.Sprintf("sha256:%x", h.Sum(nil))
	if computed != expectedDigest {
		return fmt.Errorf("digest mismatch: expected %s, got %s", expectedDigest, computed)
	}
	return nil
}

var _ = context.Background
var _ = time.Second
