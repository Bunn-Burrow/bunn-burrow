package rootfs

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
)

// Builder extracts and merges OCI image layers into a root filesystem directory.
type Builder struct {
	rootDir  string
	dirModes map[string]os.FileMode
}

// New creates a rootfs builder that merges layers into the given directory.
func New(rootDir string) *Builder {
	return &Builder{rootDir: rootDir, dirModes: make(map[string]os.FileMode)}
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
	magic := make([]byte, 2)
	if _, err := io.ReadFull(f, magic); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
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

	root, err := os.OpenRoot(b.rootDir)
	if err != nil {
		return fmt.Errorf("open rootfs directory: %w", err)
	}
	defer root.Close()

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}

		if err := b.applyEntry(root, hdr, tr); err != nil {
			return fmt.Errorf("apply entry %s: %w", hdr.Name, err)
		}
	}
	return nil
}

func (b *Builder) applyEntry(root *os.Root, hdr *tar.Header, tr io.Reader) error {
	name, err := cleanLayerPath(hdr.Name)
	if err != nil {
		return err
	}

	base := path.Base(name)
	dir := path.Dir(name)

	if base == OpaqueDirMarker {
		return b.clearDirectory(root, dir)
	}

	if strings.HasPrefix(base, WhiteoutPrefix) && len(base) > len(WhiteoutPrefix) {
		target, err := cleanLayerPath(path.Join(dir, base[len(WhiteoutPrefix):]))
		if err != nil {
			return err
		}
		return b.removePath(root, target)
	}

	switch hdr.Typeflag {
	case tar.TypeDir:
		return b.createDir(root, name, hdr)
	case tar.TypeReg, tar.TypeRegA:
		return b.createFile(root, name, hdr, tr)
	case tar.TypeSymlink:
		return b.createSymlink(root, name, hdr.Linkname)
	case tar.TypeLink:
		return b.createHardLink(root, name, hdr.Linkname)
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		return fmt.Errorf("unsupported device type %d for %s", hdr.Typeflag, name)
	default:
		return nil
	}
}

func (b *Builder) createDir(root *os.Root, name string, hdr *tar.Header) error {
	if name == "." {
		b.dirModes[name] = os.FileMode(hdr.Mode)
		return nil
	}
	parent, base, err := b.destination(root, name)
	if err != nil {
		return err
	}
	target := path.Join(parent, base)
	info, err := root.Lstat(target)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		b.forgetModes(target)
		if err := root.RemoveAll(target); err != nil {
			return err
		}
	}
	if err := root.MkdirAll(target, 0o755); err != nil {
		return err
	}
	b.dirModes[target] = os.FileMode(hdr.Mode)
	return nil
}

func (b *Builder) createFile(root *os.Root, name string, hdr *tar.Header, tr io.Reader) error {
	parent, base, err := b.destination(root, name)
	if err != nil {
		return err
	}
	target := path.Join(parent, base)
	b.forgetModes(target)
	if err := root.RemoveAll(target); err != nil {
		return err
	}

	out, err := root.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, tr)
	modeErr := out.Chmod(os.FileMode(hdr.Mode))
	closeErr := out.Close()
	if err := errors.Join(copyErr, modeErr, closeErr); err != nil {
		return err
	}
	return nil
}

func (b *Builder) createSymlink(root *os.Root, name, target string) error {
	parent, base, err := b.destination(root, name)
	if err != nil {
		return err
	}
	link := path.Join(parent, base)
	b.forgetModes(link)
	if err := root.RemoveAll(link); err != nil {
		return err
	}
	return root.Symlink(target, link)
}

func (b *Builder) createHardLink(root *os.Root, name, target string) error {
	source, err := cleanLayerPath(target)
	if err != nil {
		return err
	}
	sourceParent, sourceBase, err := b.destination(root, source)
	if err != nil {
		return err
	}
	source = path.Join(sourceParent, sourceBase)
	parent, base, err := b.destination(root, name)
	if err != nil {
		return err
	}
	link := path.Join(parent, base)
	b.forgetModes(link)
	if err := root.RemoveAll(link); err != nil {
		return err
	}
	return root.Link(source, link)
}

func (b *Builder) destination(root *os.Root, name string) (string, string, error) {
	parent, err := resolvePath(root, path.Dir(name))
	if err != nil {
		return "", "", err
	}
	if err := root.MkdirAll(parent, 0o755); err != nil {
		return "", "", err
	}
	return parent, path.Base(name), nil
}

func (b *Builder) removePath(root *os.Root, name string) error {
	if name == "." {
		return fmt.Errorf("%w: cannot remove root directory", ErrInvalidPath)
	}
	parent, base, err := b.destination(root, name)
	if err != nil {
		return err
	}
	return root.RemoveAll(path.Join(parent, base))
}

func (b *Builder) clearDirectory(root *os.Root, dir string) error {
	resolved, err := resolvePath(root, dir)
	if err != nil {
		return err
	}
	dirRoot, err := root.OpenRoot(resolved)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer dirRoot.Close()

	directory, err := dirRoot.Open(".")
	if err != nil {
		return err
	}
	entries, err := directory.ReadDir(-1)
	closeErr := directory.Close()
	if err := errors.Join(err, closeErr); err != nil {
		return err
	}
	for _, entry := range entries {
		b.forgetModes(path.Join(resolved, entry.Name()))
		if err := dirRoot.RemoveAll(entry.Name()); err != nil {
			return err
		}
	}
	return nil
}

// Finalize applies directory permissions after all layers have been merged.
func (b *Builder) Finalize() error {
	root, err := os.OpenRoot(b.rootDir)
	if err != nil {
		return fmt.Errorf("open rootfs directory: %w", err)
	}
	defer root.Close()

	dirs := make([]string, 0, len(b.dirModes))
	for dir := range b.dirModes {
		dirs = append(dirs, dir)
	}
	sort.Slice(dirs, func(i, j int) bool {
		return strings.Count(dirs[i], "/") > strings.Count(dirs[j], "/")
	})
	for _, dir := range dirs {
		if err := root.Chmod(dir, b.dirModes[dir]); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("set directory mode for %q: %w", dir, err)
		}
	}
	return nil
}

func (b *Builder) forgetModes(name string) {
	for dir := range b.dirModes {
		if dir == name || strings.HasPrefix(dir, name+"/") {
			delete(b.dirModes, dir)
		}
	}
}

func cleanLayerPath(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%w: empty path", ErrInvalidPath)
	}
	clean := path.Clean(strings.TrimLeft(name, "/"))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%w: %q", ErrInvalidPath, name)
	}
	return clean, nil
}

func resolvePath(root *os.Root, name string) (string, error) {
	name, err := cleanLayerPath(name)
	if err != nil {
		return "", err
	}
	if name == "." {
		return ".", nil
	}

	pending := strings.Split(name, "/")
	resolved := make([]string, 0, len(pending))
	symlinkCount := 0
	for i := 0; i < len(pending); i++ {
		component := pending[i]
		if component == "" || component == "." {
			continue
		}
		if component == ".." {
			if len(resolved) > 0 {
				resolved = resolved[:len(resolved)-1]
			}
			continue
		}

		candidate := path.Join(append(resolved, component)...)
		info, err := root.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			resolved = append(resolved, component)
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			resolved = append(resolved, component)
			continue
		}

		symlinkCount++
		if symlinkCount > 40 {
			return "", fmt.Errorf("%w: too many symlinks in %q", ErrInvalidPath, name)
		}
		target, err := root.Readlink(candidate)
		if err != nil {
			return "", err
		}
		if path.IsAbs(target) {
			target = strings.TrimLeft(target, "/")
		} else {
			target = path.Join(path.Join(resolved...), target)
		}
		pending = append(strings.Split(target, "/"), pending[i+1:]...)
		resolved = resolved[:0]
		i = -1
	}
	if len(resolved) == 0 {
		return ".", nil
	}
	return path.Join(resolved...), nil
}

// WhiteoutPrefix is the prefix for OCI whiteout files.
const WhiteoutPrefix = ".wh."

// OpaqueDirMarker is the marker for OCI opaque directories.
const OpaqueDirMarker = ".wh..wh..opq"

// ErrInvalidPath is returned when a layer contains an unsafe path.
var ErrInvalidPath = errors.New("invalid path in layer")
