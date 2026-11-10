package storage

import (
	"fmt"
	"os"
	"path/filepath"
)

// Manager handles sandbox filesystem isolation.
// It creates per-sandbox writable ext4 overlay files on top of an immutable
// base rootfs image.
type Manager struct {
	baseDir     string
	overlayDir  string
	overlaySize int64 // size in bytes for each overlay
}

// New creates a storage manager.
func New(baseDir, overlayDir string, overlaySize int64) *Manager {
	return &Manager{
		baseDir:     baseDir,
		overlayDir:  overlayDir,
		overlaySize: overlaySize,
	}
}

// EnsureDirs creates the required directories.
func (m *Manager) EnsureDirs() error {
	if err := os.MkdirAll(m.baseDir, 0o750); err != nil {
		return fmt.Errorf("create base dir: %w", err)
	}
	if err := os.MkdirAll(m.overlayDir, 0o750); err != nil {
		return fmt.Errorf("create overlay dir: %w", err)
	}
	return nil
}

// BasePath returns the path to the immutable base rootfs image.
func (m *Manager) BasePath() string {
	return filepath.Join(m.baseDir, "base.ext4")
}

// OverlayPath returns the path to a sandbox's writable overlay file.
func (m *Manager) OverlayPath(sandboxID string) string {
	return filepath.Join(m.overlayDir, sandboxID+".ext4")
}

// CreateOverlay creates a new sparse ext4 overlay file for a sandbox.
// Returns the path to the overlay file.
func (m *Manager) CreateOverlay(sandboxID string) (string, error) {
	if err := os.MkdirAll(m.overlayDir, 0o750); err != nil {
		return "", fmt.Errorf("create overlay dir: %w", err)
	}

	overlayPath := m.OverlayPath(sandboxID)

	// Check if overlay already exists.
	if _, err := os.Stat(overlayPath); err == nil {
		return overlayPath, nil // already exists
	} else if !os.IsNotExist(err) {
		return "", err
	}

	// Create a sparse file of the specified size.
	f, err := os.OpenFile(overlayPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("create overlay file: %w", err)
	}

	if err := f.Truncate(m.overlaySize); err != nil {
		f.Close()
		os.Remove(overlayPath)
		return "", fmt.Errorf("truncate overlay: %w", err)
	}
	f.Close()

	return overlayPath, nil
}

// RemoveOverlay removes a sandbox's overlay file.
func (m *Manager) RemoveOverlay(sandboxID string) error {
	overlayPath := m.OverlayPath(sandboxID)
	if err := os.Remove(overlayPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ListOverlays returns all overlay file paths.
func (m *Manager) ListOverlays() ([]string, error) {
	entries, err := os.ReadDir(m.overlayDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var paths []string
	for _, e := range entries {
		if !e.IsDir() {
			paths = append(paths, filepath.Join(m.overlayDir, e.Name()))
		}
	}
	return paths, nil
}

// OverlaySize returns the configured overlay size in bytes.
func (m *Manager) OverlaySize() int64 {
	return m.overlaySize
}

// BaseDir returns the base directory for prepared base images.
func (m *Manager) BaseDir() string {
	return m.baseDir
}

// DefaultOverlaySize returns a reasonable default overlay size (10 GiB).
const DefaultOverlaySize = int64(10 * 1024 * 1024 * 1024)
