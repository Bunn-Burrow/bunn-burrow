package image

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Bunn-Burrow/bunn-burrow/internal/template"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Manager resolves OCI images, downloads manifests and layers.
type Manager struct {
	cacheDir string
}

// NewManager creates an image manager with the given cache directory.
func NewManager(cacheDir string) *Manager {
	return &Manager{cacheDir: cacheDir}
}

// InspectImage resolves an image reference and returns basic image info.
// It implements template.ImageResolver.
func (m *Manager) InspectImage(id string) (*template.ImageInfo, error) {
	ref, err := name.ParseReference(id)
	if err != nil {
		return nil, fmt.Errorf("parse image reference %q: %w", id, err)
	}
	if _, err := remote.Get(ref); err != nil {
		return nil, fmt.Errorf("inspect image %q: %w", id, err)
	}
	return &template.ImageInfo{
		ID:    id,
		State: "active",
	}, nil
}

// Resolve resolves an image reference to a v1.Image.
func (m *Manager) Resolve(ctx context.Context, ref *Reference) (v1.Image, error) {
	remoteRef, err := name.ParseReference(ref.RemoteName(), name.WithDefaultRegistry(ref.Domain()))
	if err != nil {
		return nil, fmt.Errorf("parse reference: %w", err)
	}

	// If digest is specified, resolve by digest
	if ref.Digest != "" {
		desc, err := remote.Get(remoteRef, remote.WithContext(ctx))
		if err != nil {
			return nil, fmt.Errorf("resolve image by digest: %w", err)
		}
		return desc.Image()
	}

	// Resolve by tag
	tagRef, ok := remoteRef.(name.Tag)
	if !ok {
		return nil, fmt.Errorf("reference %s is not a tag", ref.String())
	}

	desc, err := remote.Get(tagRef, remote.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("resolve image by tag: %w", err)
	}
	return desc.Image()
}

// Manifest returns the image manifest.
func Manifest(img v1.Image) (*v1.Manifest, error) {
	return img.Manifest()
}

// Config returns the image configuration.
func Config(img v1.Image) (*v1.ConfigFile, error) {
	return img.ConfigFile()
}

// Layers returns the image layers in order (bottom to top).
func Layers(img v1.Image) ([]v1.Layer, error) {
	return img.Layers()
}

// DownloadLayer downloads a layer's compressed tar stream and writes it to dst.
func DownloadLayer(ctx context.Context, layer v1.Layer, dst io.Writer) error {
	rc, err := layer.Compressed()
	if err != nil {
		return fmt.Errorf("open compressed layer: %w", err)
	}
	defer rc.Close()

	if _, err := io.Copy(dst, rc); err != nil {
		return fmt.Errorf("copy layer: %w", err)
	}
	return nil
}

// LayerDigest returns the layer's digest.
func LayerDigest(layer v1.Layer) (string, error) {
	digest, err := layer.Digest()
	if err != nil {
		return "", fmt.Errorf("layer digest: %w", err)
	}
	return digest.String(), nil
}

// LayerSize returns the compressed size of a layer.
func LayerSize(layer v1.Layer) (int64, error) {
	size, err := layer.Size()
	if err != nil {
		return 0, fmt.Errorf("layer size: %w", err)
	}
	return size, nil
}

// EnsureCacheDir creates the cache directory if it doesn't exist.
func EnsureCacheDir(dir string) error {
	return os.MkdirAll(dir, 0o750)
}

// Platform returns the default platform for the image manager.
func Platform() v1.Platform {
	return v1.Platform{Architecture: "amd64", OS: "linux"}
}

// PathOnHost returns the filesystem path where a cached blob is stored.
func PathOnHost(cacheDir, digest string) string {
	return filepath.Join(cacheDir, "blobs", digest)
}

// BlobExists checks if a blob with the given digest exists in the cache.
func BlobExists(cacheDir, digest string) bool {
	_, err := os.Stat(PathOnHost(cacheDir, digest))
	return err == nil
}
