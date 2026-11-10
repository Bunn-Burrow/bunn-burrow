package rootfs

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Builder extracts and merges OCI image layers into a root filesystem directory.
type Builder struct {
	rootDir string
}

// New creates a rootfs builder that merges layers into the given directory.
func New(rootDir string) *Builder {
	return &Builder{rootDir: rootDir}
}

// PrepareDir creates the root directory if it doesn't exist.
func (b *Builder) PrepareDir() error {
	return os.MkdirAll(b.rootDir, 0o755)
}

// ApplyLayer extracts a compressed tar layer into the root directory,
// applying OCI whiteout semantics.
func (b *Builder) ApplyLayer(layerPath string) error {
	f, err := os.Open(layerPath)
	if err != nil {
		return fmt.Errorf("open layer: %w", err)
	}
	defer f.Close()

	var tr *tar.Reader
	// Detect gzip by magic bytes.
	magic := make([]byte, 2)
	if _, err := f.Read(magic); err != nil {
		return fmt.Errorf("read magic: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek: %w", err)
	}

	if magic[0] == 0x1f && magic[1] == 0x8b {
		gr, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("gzip reader: %w", err)
		}
		defer gr.Close()
		tr = tar.NewReader(gr)
	} else {
		tr = tar.NewReader(f)
	}

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}

		if err := b.applyEntry(hdr, tr); err != nil {
			return fmt.Errorf("apply entry %s: %w", hdr.Name, err)
		}
	}
	return nil
}

func (b *Builder) applyEntry(hdr *tar.Header, tr io.Reader) error {
	name := hdr.Name

	// Reject path traversal.
	if strings.Contains(name, "..") || filepath.IsAbs(name) {
		return fmt.Errorf("unsafe path: %s", name)
	}

	// Clean the name.
	name = filepath.Clean(name)

	// Check for whiteout prefix.
	base := filepath.Base(name)
	dir := filepath.Dir(name)

	// OCI whiteout file: .wh.<filename>
	if strings.HasPrefix(base, WhiteoutPrefix) && len(base) > len(WhiteoutPrefix) {
		target := filepath.Join(dir, base[len(WhiteoutPrefix):])
		return b.removePath(target)
	}

	// OCI opaque directory marker: .wh..wh..opq
	if base == OpaqueDirMarker {
		// Remove all existing contents of this directory (but not the dir itself).
		return b.clearDirectory(dir)
	}

	targetPath := filepath.Join(b.rootDir, name)

	switch hdr.Typeflag {
	case tar.TypeDir:
		return b.createDir(targetPath, hdr)
	case tar.TypeReg, tar.TypeRegA:
		return b.createFile(targetPath, hdr, tr)
	case tar.TypeSymlink:
		return b.createSymlink(targetPath, hdr.Linkname)
	case tar.TypeLink:
		return b.createHardLink(targetPath, hdr.Linkname, b.rootDir)
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		// Device nodes and fifos are not supported in guest rootfs.
		return fmt.Errorf("unsupported device type %d for %s", hdr.Typeflag, name)
	default:
		// Skip unknown types.
		return nil
	}
}

func (b *Builder) createDir(path string, hdr *tar.Header) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(path, os.FileMode(hdr.Mode)); err != nil {
		return err
	}
	return nil
}

func (b *Builder) createFile(path string, hdr *tar.Header, tr io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, tr); err != nil {
		return err
	}

	if err := out.Chmod(os.FileMode(hdr.Mode)); err != nil {
		return err
	}
	return nil
}

func (b *Builder) createSymlink(path, target string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.Symlink(target, path)
}

func (b *Builder) createHardLink(path, target string, rootDir string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	targetPath := filepath.Join(rootDir, filepath.Clean(target))
	return os.Link(targetPath, path)
}

func (b *Builder) removePath(path string) error {
	fullPath := filepath.Join(b.rootDir, path)
	if err := os.RemoveAll(fullPath); err != nil {
		return err
	}
	return nil
}

func (b *Builder) clearDirectory(dir string) error {
	fullDir := filepath.Join(b.rootDir, dir)
	entries, err := os.ReadDir(fullDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(fullDir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// WhiteoutPrefix is the prefix for OCI whiteout files.
const WhiteoutPrefix = ".wh."

// OpaqueDirMarker is the marker for OCI opaque directories.
const OpaqueDirMarker = ".wh..wh..opq"

// ErrInvalidPath is returned when a layer contains an unsafe path.
var ErrInvalidPath = errors.New("invalid path in layer")
