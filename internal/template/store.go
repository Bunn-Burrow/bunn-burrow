package template

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// metadataDir holds template metadata JSON files.
	metadataDir = "metadata"
	// rootfsDir holds immutable template rootfs images.
	rootfsDir = "rootfs"
)

// Store manages template metadata and artifacts on disk. It is safe for
// concurrent use. Templates are persisted as metadata JSON files plus an
// immutable rootfs image copy; reference counts track live VM usage so a
// template in use cannot be deleted.
type Store struct {
	dir       string
	mu        sync.Mutex
	templates map[string]*Template
	refCount  map[string]int
}

// New creates a Store at dir. Existing template metadata on disk is discoverable
// via Get/List; in-memory caches are populated lazily and on Create.
func New(dir string) *Store {
	return &Store{
		dir:       dir,
		templates: make(map[string]*Template),
		refCount:  make(map[string]int),
	}
}

// EnsureDirs creates the required directories.
func (s *Store) EnsureDirs() error {
	if err := os.MkdirAll(filepath.Join(s.dir, metadataDir), 0o750); err != nil {
		return fmt.Errorf("create metadata dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(s.dir, rootfsDir), 0o750); err != nil {
		return fmt.Errorf("create rootfs dir: %w", err)
	}
	return nil
}

// Create registers a template. If ImageID is set, the template references an
// existing image (the image must already be registered with an ImageManager;
// this store does not validate the reference). If ImageID is empty, the source
// rootfs is copied into the template store as before (backward compatible).
// Returns an error if id already exists.
func (s *Store) Create(tmpl *Template) error {
	if tmpl.ID == "" {
		return fmt.Errorf("template id is required")
	}
	if filepath.Base(tmpl.ID) != tmpl.ID {
		return fmt.Errorf("template id %q must not contain path separators", tmpl.ID)
	}

	s.mu.Lock()
	if _, exists := s.templates[tmpl.ID]; exists {
		s.mu.Unlock()
		return fmt.Errorf("template %q already exists", tmpl.ID)
	}
	s.mu.Unlock()

	now := time.Now()
	if tmpl.CreatedAt.IsZero() {
		tmpl.CreatedAt = now
	}

	// If no image reference, copy the source rootfs (backward compatible).
	if tmpl.ImageID == "" {
		if tmpl.RootfsPath == "" {
			return fmt.Errorf("template %q has no image_id and no rootfs_path", tmpl.ID)
		}
		if _, err := os.Stat(tmpl.RootfsPath); err != nil {
			return fmt.Errorf("source rootfs not statable: %w", err)
		}

		tmplRootfsDir := filepath.Join(s.dir, rootfsDir, tmpl.ID)
		if err := os.MkdirAll(tmplRootfsDir, 0o750); err != nil {
			return fmt.Errorf("create template rootfs dir: %w", err)
		}

		tmpPath := filepath.Join(tmplRootfsDir, "rootfs.ext4.tmp")
		finalPath := filepath.Join(tmplRootfsDir, "rootfs.ext4")
		if err := copyFileAtomic(tmpl.RootfsPath, tmpPath, finalPath); err != nil {
			os.Remove(tmpPath)
			return fmt.Errorf("copy rootfs: %w", err)
		}

		if err := os.Chmod(finalPath, 0o444); err != nil {
			os.Remove(finalPath)
			return fmt.Errorf("chmod rootfs read-only: %w", err)
		}

		checksum, err := sha256File(finalPath)
		if err != nil {
			os.Remove(finalPath)
			return fmt.Errorf("checksum rootfs: %w", err)
		}

		tmpl.RootfsPath = finalPath
		tmpl.RootfsChecksum = checksum
		if tmpl.RootfsFormat == "" {
			tmpl.RootfsFormat = "ext4"
		}
		tmpl.Immutable = true
	}

	// Persist metadata atomically.
	metadataDirPath := filepath.Join(s.dir, metadataDir)
	if err := os.MkdirAll(metadataDirPath, 0o750); err != nil {
		return fmt.Errorf("create metadata dir: %w", err)
	}
	metaTmp := filepath.Join(metadataDirPath, tmpl.ID+".json.tmp")
	metaFinal := filepath.Join(metadataDirPath, tmpl.ID+".json")
	if err := writeJSONAtomically(metaTmp, metaFinal, tmpl); err != nil {
		if tmpl.ImageID == "" {
			os.Remove(filepath.Join(s.dir, rootfsDir, tmpl.ID, "rootfs.ext4"))
		}
		return fmt.Errorf("write metadata: %w", err)
	}

	s.mu.Lock()
	s.templates[tmpl.ID] = tmpl
	s.mu.Unlock()

	return nil
}

// Get returns the template metadata for id, or nil if not found. Metadata is
// read from disk so the store remains valid across process restarts.
func (s *Store) Get(id string) *Template {
	s.mu.Lock()
	tmpl, ok := s.templates[id]
	s.mu.Unlock()
	if ok {
		return tmpl
	}
	return s.getFromDisk(id)
}

func (s *Store) getFromDisk(id string) *Template {
	data, err := os.ReadFile(s.metadataPath(id))
	if err != nil {
		return nil
	}
	var tmpl Template
	if err := json.Unmarshal(data, &tmpl); err != nil {
		return nil
	}
	if _, err := os.Stat(tmpl.RootfsPath); err != nil {
		return nil // rootfs missing, treat as not registered
	}
	return &tmpl
}

// List returns all registered templates.
func (s *Store) List() ([]*Template, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, metadataDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var templates []*Template
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, metadataDir, e.Name()))
		if err != nil {
			continue
		}
		var tmpl Template
		if err := json.Unmarshal(data, &tmpl); err != nil {
			continue
		}
		templates = append(templates, &tmpl)
	}
	return templates, nil
}

// Validate verifies the template rootfs checksum matches the stored metadata.
// Returns an error if the template is unknown or the rootfs has been modified.
func (s *Store) Validate(id string) error {
	tmpl := s.Get(id)
	if tmpl == nil {
		return fmt.Errorf("template %q not found", id)
	}
	checksum, err := sha256File(tmpl.RootfsPath)
	if err != nil {
		return fmt.Errorf("checksum rootfs: %w", err)
	}
	if checksum != tmpl.RootfsChecksum {
		return fmt.Errorf("template %q rootfs checksum mismatch: expected %s, got %s", id, tmpl.RootfsChecksum, checksum)
	}
	return nil
}

// Delete removes a template and its artifacts. It fails if any VM currently
// uses the template (reference count > 0). Cleanup errors are collected and
// returned; partial removal is surfaced rather than silently ignored.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	if s.refCount[id] > 0 {
		s.mu.Unlock()
		return fmt.Errorf("template %q is in use by %d VM(s), refusing to delete", id, s.refCount[id])
	}
	delete(s.refCount, id)
	s.mu.Unlock()

	tmpl := s.Get(id)
	var errs []error
	if tmpl != nil && tmpl.RootfsPath != "" {
		if err := os.RemoveAll(filepath.Dir(tmpl.RootfsPath)); err != nil {
			errs = append(errs, fmt.Errorf("remove rootfs: %w", err))
		}
	}
	metaPath := s.metadataPath(id)
	if err := os.Remove(metaPath); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("remove metadata: %w", err))
	}

	s.mu.Lock()
	delete(s.templates, id)
	s.mu.Unlock()

	if len(errs) > 0 {
		return fmt.Errorf("delete template %q: %w", id, errors.Join(errs...))
	}
	return nil
}

// Acquire increments the template's in-use reference count. Returns an error if
// the template is unknown.
func (s *Store) Acquire(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.templates[id]; !ok {
		if s.getFromDisk(id) == nil {
			return fmt.Errorf("template %q not found", id)
		}
	}
	s.refCount[id]++
	return nil
}

// Release decrements the template's in-use reference count, floored at zero.
func (s *Store) Release(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refCount[id] > 0 {
		s.refCount[id]--
	}
}

func (s *Store) metadataPath(id string) string {
	return filepath.Join(s.dir, metadataDir, id+".json")
}

// copyFileAtomic copies src to tmpPath then renames to finalPath. On failure
// the temp file is removed so no partial artifact remains.
func copyFileAtomic(src, tmpPath, finalPath string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// writeJSONAtomically marshals v to tmpPath then renames to finalPath.
func writeJSONAtomically(tmpPath, finalPath string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, finalPath)
}

// sha256File returns the SHA256 hex digest of the file at path.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
