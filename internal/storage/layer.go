package storage

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// LayerManager creates and removes per-VM writable filesystem layers derived
// from an immutable template rootfs. Each layer is an independent copy of the
// template rootfs owned by a single VM. Changes made by one VM are confined to
// its own layer and are not visible to other VMs or to the template.
type LayerManager struct {
	rootDir string
}

// NewLayerManager creates a LayerManager that stores writable layers under
// rootDir. Each VM's layer lives at <rootDir>/<vmID>/rootfs.ext4.
func NewLayerManager(rootDir string) *LayerManager {
	return &LayerManager{rootDir: rootDir}
}

// EnsureDirs creates the required directories.
func (m *LayerManager) EnsureDirs() error {
	return os.MkdirAll(m.rootDir, 0o750)
}

// LayerPath returns the path to a VM's writable layer file.
func (m *LayerManager) LayerPath(vmID string) string {
	return filepath.Join(m.rootDir, vmID, "rootfs.ext4")
}

// CreateLayer creates a writable layer for vmID by copying the template rootfs
// at templateRootfsPath. The copy is atomic: written to a temp file then
// renamed into place so a failed creation never leaves a partial layer. Returns
// the layer path. The template rootfs is only read, never modified.
func (m *LayerManager) CreateLayer(templateRootfsPath, vmID string) (string, error) {
	if err := os.MkdirAll(filepath.Join(m.rootDir, vmID), 0o750); err != nil {
		return "", fmt.Errorf("create vm layer dir: %w", err)
	}

	layerPath := m.LayerPath(vmID)
	tmpPath := layerPath + ".tmp"

	in, err := os.Open(templateRootfsPath)
	if err != nil {
		return "", fmt.Errorf("open template rootfs: %w", err)
	}
	defer in.Close()

	out, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("create layer file: %w", err)
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("copy rootfs to layer: %w", err)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("sync layer: %w", err)
	}
	if err := out.Close(); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("close layer: %w", err)
	}

	if err := os.Rename(tmpPath, layerPath); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("rename layer: %w", err)
	}

	return layerPath, nil
}

// RemoveLayer removes a VM's writable layer file and its directory. It is
// idempotent: removing a missing layer is not an error.
func (m *LayerManager) RemoveLayer(vmID string) error {
	dir := filepath.Join(m.rootDir, vmID)
	if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
