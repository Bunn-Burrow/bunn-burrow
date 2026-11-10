package envcache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// EnvironmentCache stores prepared Firecracker-ready environments.
// Each environment is identified by a deterministic cache key based on the
// resolved image digest, platform, and Guest Agent version.
type EnvironmentCache struct {
	dir   string
	mu    sync.Mutex
	builds map[string]*buildTracker
}

// buildTracker tracks an in-flight environment build.
type buildTracker struct {
	done  chan struct{}
	err   error
	env   *Environment
}

// Environment describes a prepared environment ready for Firecracker boot.
type Environment struct {
	CacheKey          string    `json:"cache_key"`
	ImageReference    string    `json:"image_reference"`
	ImageDigest       string    `json:"image_digest"`
	Platform          string    `json:"platform"`
	RootfsPath        string    `json:"rootfs_path"`
	GuestAgentVersion string    `json:"guest_agent_version"`
	RootfsFormat      string    `json:"rootfs_format"`
	ConfigDigest      string    `json:"config_digest"`
	CreatedAt         time.Time `json:"created_at"`
	SizeBytes         int64     `json:"size_bytes"`
}

// New creates an EnvironmentCache at the given directory.
func New(dir string) *EnvironmentCache {
	return &EnvironmentCache{
		dir:    dir,
		builds: make(map[string]*buildTracker),
	}
}

// EnsureDir creates the cache directory structure.
func (c *EnvironmentCache) EnsureDir() error {
	if err := os.MkdirAll(c.dir, 0o750); err != nil {
		return fmt.Errorf("create envcache dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(c.dir, "environments"), 0o750); err != nil {
		return fmt.Errorf("create environments dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(c.dir, "metadata"), 0o750); err != nil {
		return fmt.Errorf("create metadata dir: %w", err)
	}
	return nil
}

// CacheKey computes a deterministic cache key from the image digest,
// platform, and guest agent version.
func CacheKey(imageDigest, platform, guestAgentVersion string) string {
	return fmt.Sprintf("%s:%s:%s", imageDigest, platform, guestAgentVersion)
}

// Get returns a cached environment if it exists, or nil.
// If a build is in-flight for the same cache key, waits for it to complete
// and then reads the result from disk. Does not hold the mutex while waiting,
// avoiding deadlocks with the finish callback.
func (c *EnvironmentCache) Get(cacheKey string) *Environment {
	c.mu.Lock()
	if tracker, ok := c.builds[cacheKey]; ok {
		ch := tracker.done
		c.mu.Unlock()
		<-ch
		// Build completed; read result from disk.
		return c.getFromDisk(cacheKey)
	}
	env := c.getFromDisk(cacheKey)
	c.mu.Unlock()
	return env
}

// getFromDisk reads environment metadata from disk. Caller must hold c.mu.
func (c *EnvironmentCache) getFromDisk(cacheKey string) *Environment {
	metaPath := c.metadataPath(cacheKey)
	data, err := os.ReadFile(metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return nil
	}
	var env Environment
	if err := json.Unmarshal(data, &env); err != nil {
		return nil
	}
	// Verify the rootfs still exists.
	if _, err := os.Stat(env.RootfsPath); err != nil {
		if os.IsNotExist(err) {
			return nil // rootfs missing, treat as not cached
		}
		return nil
	}
	return &env
}

// Store stores a prepared environment in the cache.
// If a build tracker exists for the same cache key and its env is nil
// (build not yet completed), storing is allowed because the caller is
// the builder completing its own build.
func (c *EnvironmentCache) Store(env *Environment) error {
	c.mu.Lock()
	// Allow storing when the caller owns the in-flight build (tracker exists
	// but env is nil, meaning build hasn't published yet) or when storing
	// the same env that's already tracked. Reject only if a different
	// builder is in progress with a different env.
	if tracker, ok := c.builds[env.CacheKey]; ok && tracker.env != nil && tracker.env != env {
		c.mu.Unlock()
		return fmt.Errorf("build already in progress for %s", env.CacheKey)
	}
	c.mu.Unlock()

	envDir := c.envDirPath(env.CacheKey)
	if err := os.MkdirAll(envDir, 0o750); err != nil {
		return fmt.Errorf("create env dir: %w", err)
	}

	metaPath := c.metadataPath(env.CacheKey)
	if err := os.MkdirAll(filepath.Dir(metaPath), 0o750); err != nil {
		return fmt.Errorf("create metadata dir: %w", err)
	}
	data, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal environment: %w", err)
	}
	if err := os.WriteFile(metaPath, data, 0o600); err != nil {
		return fmt.Errorf("write metadata: %w", err)
	}

	return nil
}

// BeginBuild marks a build as in-flight for the given cache key.
// Returns true if this caller should perform the build, false if another
// caller is already building it (in which case the caller should wait).
// The returned finish function must be called exactly once by the builder
// to publish the result and wake waiters.
func (c *EnvironmentCache) BeginBuild(cacheKey string) (bool, func(error, *Environment)) {
	c.mu.Lock()
	if tracker, ok := c.builds[cacheKey]; ok {
		ch := tracker.done
		c.mu.Unlock()
		<-ch
		return false, func(err error, env *Environment) {} // no-op; original builder handles completion
	}

	tracker := &buildTracker{done: make(chan struct{})}
	c.builds[cacheKey] = tracker
	c.mu.Unlock()

	return true, func(err error, env *Environment) {
		c.mu.Lock()
		tracker.err = err
		tracker.env = env
		delete(c.builds, cacheKey)
		close(tracker.done)
		c.mu.Unlock()
	}
}

// metadataPath returns the path to the environment metadata file.
func (c *EnvironmentCache) metadataPath(cacheKey string) string {
	return filepath.Join(c.dir, "metadata", safeCacheKey(cacheKey)+".json")
}

// envDirPath returns the environment directory path.
func (c *EnvironmentCache) envDirPath(cacheKey string) string {
	return filepath.Join(c.dir, "environments", safeCacheKey(cacheKey))
}

// SafeCacheKey returns a filesystem-safe version of the cache key.
func SafeCacheKey(key string) string {
	return safeCacheKey(key)
}

func safeCacheKey(key string) string {
	var sb strings.Builder
	for _, r := range key {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			sb.WriteRune(r)
		} else {
			sb.WriteRune('_')
		}
	}
	return sb.String()
}

// List returns all cached environments.
func (c *EnvironmentCache) List() ([]*Environment, error) {
	metaDir := filepath.Join(c.dir, "metadata")
	entries, err := os.ReadDir(metaDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var envs []*Environment
	for _, e := range entries {
		if e.IsDir() || !stringsSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(metaDir, e.Name()))
		if err != nil {
			continue
		}
		var env Environment
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		envs = append(envs, &env)
	}
	return envs, nil
}

// Remove deletes a cached environment.
func (c *EnvironmentCache) Remove(cacheKey string) error {
	c.mu.Lock()
	delete(c.builds, cacheKey)
	c.mu.Unlock()

	metaPath := c.metadataPath(cacheKey)
	if err := os.Remove(metaPath); err != nil && !os.IsNotExist(err) {
		return err
	}

	envDir := c.envDirPath(cacheKey)
	if err := os.RemoveAll(envDir); err != nil {
		return err
	}
	return nil
}

// Has checks if an environment exists in the cache.
func (c *EnvironmentCache) Has(cacheKey string) bool {
	return c.Get(cacheKey) != nil
}

func stringsSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
