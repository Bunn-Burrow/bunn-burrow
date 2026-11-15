package image

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Bunn-Burrow/bunn-burrow/internal/storage"
	"github.com/stretchr/testify/require"
)

func TestImageConfigDefaults(t *testing.T) {
	cfg := DefaultImageConfig(t.TempDir())
	require.Greater(t, cfg.MaxImageSizeBytes, int64(0))
}

func TestImageManagerCreateAndInspect(t *testing.T) {
	dir := t.TempDir()
	metaStore := storage.NewMetadataStore(dir)
	require.NoError(t, metaStore.EnsureDirs())
	require.NoError(t, metaStore.EnsureDirs())
	mgr := NewImageManager(DefaultImageConfig(dir), metaStore)
	require.NoError(t, mgr.EnsureDirs())

	// Create a small test image file.
	imgPath := filepath.Join(dir, "test.img")
	require.NoError(t, os.WriteFile(imgPath, []byte("test image data"), 0o644))

	img, err := mgr.CreateImage("img-1", "test-image", "1.0", imgPath, ImageFormatQCOW2, 0, nil, nil)
	require.NoError(t, err)
	require.Equal(t, "img-1", img.ID)
	require.Equal(t, "test-image", img.Name)
	require.Equal(t, "1.0", img.Version)
	require.Equal(t, ImageFormatQCOW2, img.Format)
	require.Equal(t, ImageStateActive, img.State)
	require.NotEmpty(t, img.Path)
	require.NotZero(t, img.SizeBytes)
	require.NotEmpty(t, img.Checksum)
	require.NotZero(t, img.CreatedAt)

	// Inspect should return the same image.
	inspected, err := mgr.InspectImage("img-1")
	require.NoError(t, err)
	require.NotNil(t, inspected)
	require.Equal(t, img.ID, inspected.ID)
	require.Equal(t, img.SizeBytes, inspected.SizeBytes)
	require.Equal(t, img.State, inspected.State)
	require.Equal(t, img.Checksum, inspected.Checksum)
}

func TestImageManagerCreateDuplicate(t *testing.T) {
	dir := t.TempDir()
	metaStore := storage.NewMetadataStore(dir)
	require.NoError(t, metaStore.EnsureDirs())
	mgr := NewImageManager(DefaultImageConfig(dir), metaStore)

	imgPath := filepath.Join(dir, "test.img")
	require.NoError(t, os.WriteFile(imgPath, []byte("data"), 0o644))

	_, err := mgr.CreateImage("dup-img", "dup", "1.0", imgPath, ImageFormatQCOW2, 0, nil, nil)
	require.NoError(t, err)

	_, err = mgr.CreateImage("dup-img", "dup", "1.0", imgPath, ImageFormatQCOW2, 0, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "already exists")
}

func TestImageManagerCreateInvalidID(t *testing.T) {
	dir := t.TempDir()
	metaStore := storage.NewMetadataStore(dir)
	require.NoError(t, metaStore.EnsureDirs())
	mgr := NewImageManager(DefaultImageConfig(dir), metaStore)

	imgPath := filepath.Join(dir, "test.img")
	require.NoError(t, os.WriteFile(imgPath, []byte("data"), 0o644))

	_, err := mgr.CreateImage("", "test", "1.0", imgPath, ImageFormatQCOW2, 0, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "image id is required")

	_, err = mgr.CreateImage("../escape", "test", "1.0", imgPath, ImageFormatQCOW2, 0, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "path separators")

	_, err = mgr.CreateImage("..", "test", "1.0", imgPath, ImageFormatQCOW2, 0, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "reserved")
}

func TestImageManagerCreateSizeValidation(t *testing.T) {
	dir := t.TempDir()
	metaStore := storage.NewMetadataStore(dir)
	require.NoError(t, metaStore.EnsureDirs())
	mgr := NewImageManager(DefaultImageConfig(dir), metaStore)

	imgPath := filepath.Join(dir, "test.img")
	require.NoError(t, os.WriteFile(imgPath, []byte("data"), 0o644))

	// Too large.
	_, err := mgr.CreateImage("large-img", "large", "1.0", imgPath, ImageFormatQCOW2, mgr.cfg.MaxImageSizeBytes+1, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds maximum")

	// Zero uses actual file size.
	img, err := mgr.CreateImage("auto-size-img", "auto", "1.0", imgPath, ImageFormatQCOW2, 0, nil, nil)
	require.NoError(t, err)
	require.Greater(t, img.SizeBytes, int64(0))
}

func TestImageManagerListImages(t *testing.T) {
	dir := t.TempDir()
	metaStore := storage.NewMetadataStore(dir)
	require.NoError(t, metaStore.EnsureDirs())
	mgr := NewImageManager(DefaultImageConfig(dir), metaStore)
	require.NoError(t, mgr.EnsureDirs())

	// Empty list initially.
	imgs, err := mgr.ListImages()
	require.NoError(t, err)
	require.Len(t, imgs, 0)

	// Create some images.
	imgPath := filepath.Join(dir, "test.img")
	require.NoError(t, os.WriteFile(imgPath, []byte("data"), 0o644))

	_, err = mgr.CreateImage("img-a", "img-a", "1.0", imgPath, ImageFormatQCOW2, 0, nil, nil)
	require.NoError(t, err)
	_, err = mgr.CreateImage("img-b", "img-b", "2.0", imgPath, ImageFormatQCOW2, 0, nil, nil)
	require.NoError(t, err)

	imgs, err = mgr.ListImages()
	require.NoError(t, err)
	require.Len(t, imgs, 2)

	ids := map[string]bool{}
	for _, img := range imgs {
		ids[img.ID] = true
	}
	require.True(t, ids["img-a"])
	require.True(t, ids["img-b"])
}

func TestImageManagerDeleteImage(t *testing.T) {
	dir := t.TempDir()
	metaStore := storage.NewMetadataStore(dir)
	require.NoError(t, metaStore.EnsureDirs())
	mgr := NewImageManager(DefaultImageConfig(dir), metaStore)
	require.NoError(t, mgr.EnsureDirs())

	imgPath := filepath.Join(dir, "test.img")
	require.NoError(t, os.WriteFile(imgPath, []byte("data"), 0o644))

	img, err := mgr.CreateImage("del-img", "del", "1.0", imgPath, ImageFormatQCOW2, 0, nil, nil)
	require.NoError(t, err)
	require.FileExists(t, img.Path)

	// Delete should succeed.
	require.NoError(t, mgr.DeleteImage("del-img"))

	// Image should no longer be found.
	inspected, err := mgr.InspectImage("del-img")
	require.NoError(t, err)
	require.Nil(t, inspected)

	// Image file should be removed.
	require.NoFileExists(t, img.Path)

	// Deleting again should fail (not found).
	err = mgr.DeleteImage("del-img")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestImageManagerInvalidFormat(t *testing.T) {
	dir := t.TempDir()
	metaStore := storage.NewMetadataStore(dir)
	require.NoError(t, metaStore.EnsureDirs())
	mgr := NewImageManager(DefaultImageConfig(dir), metaStore)

	imgPath := filepath.Join(dir, "test.img")
	require.NoError(t, os.WriteFile(imgPath, []byte("data"), 0o644))

	_, err := mgr.CreateImage("bad-fmt", "bad", "1.0", imgPath, "iso", 0, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported image format")
}

func TestImageManagerVersionLookup(t *testing.T) {
	dir := t.TempDir()
	metaStore := storage.NewMetadataStore(dir)
	require.NoError(t, metaStore.EnsureDirs())
	mgr := NewImageManager(DefaultImageConfig(dir), metaStore)
	require.NoError(t, mgr.EnsureDirs())

	imgPath := filepath.Join(dir, "test.img")
	require.NoError(t, os.WriteFile(imgPath, []byte("data"), 0o644))

	img, err := mgr.CreateImage("ver-img", "ver-img", "1.0", imgPath, ImageFormatQCOW2, 0, nil, nil)
	require.NoError(t, err)

	// Lookup by name@version.
	inspected, err := mgr.InspectImageByNameVersion("ver-img", "1.0")
	require.NoError(t, err)
	require.NotNil(t, inspected)
	require.Equal(t, img.ID, inspected.ID)
	require.Equal(t, "1.0", inspected.Version)

	// Lookup non-existent version.
	inspected, err = mgr.InspectImageByNameVersion("ver-img", "2.0")
	require.NoError(t, err)
	require.Nil(t, inspected)
}

func TestImageManagerDeriveImage(t *testing.T) {
	dir := t.TempDir()
	metaStore := storage.NewMetadataStore(dir)
	require.NoError(t, metaStore.EnsureDirs())
	mgr := NewImageManager(DefaultImageConfig(dir), metaStore)
	require.NoError(t, mgr.EnsureDirs())

	imgPath := filepath.Join(dir, "base.img")
	require.NoError(t, os.WriteFile(imgPath, []byte("base image data"), 0o644))

	base, err := mgr.CreateImage("base-img", "base", "1.0", imgPath, ImageFormatQCOW2, 0, nil, nil)
	require.NoError(t, err)

	// Derive a new image from the base.
	derived, err := mgr.DeriveImage("derived-img", "derived", "1.0", "base-img", ImageFormatQCOW2, nil, nil)
	require.NoError(t, err)
	require.Equal(t, "derived-img", derived.ID)
	require.Equal(t, "derived", derived.Name)
	require.Equal(t, base.SizeBytes, derived.SizeBytes)
	require.Equal(t, "base-img", derived.ParentID)

	// Derive from non-existent image.
	_, err = mgr.DeriveImage("bad-img", "bad", "1.0", "nonexistent", ImageFormatQCOW2, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestImageManagerRecover(t *testing.T) {
	dir := t.TempDir()
	metaStore := storage.NewMetadataStore(dir)
	require.NoError(t, metaStore.EnsureDirs())

	mgr1 := NewImageManager(DefaultImageConfig(dir), metaStore)
	require.NoError(t, mgr1.EnsureDirs())

	imgPath := filepath.Join(dir, "recover.img")
	require.NoError(t, os.WriteFile(imgPath, []byte("data"), 0o644))

	_, err := mgr1.CreateImage("recover-img", "recover", "1.0", imgPath, ImageFormatQCOW2, 0, nil, nil)
	require.NoError(t, err)

	// Create a new ImageManager (simulating process restart) and recover.
	mgr2 := NewImageManager(DefaultImageConfig(dir), metaStore)
	require.NoError(t, mgr2.Recover())

	inspected, err := mgr2.InspectImage("recover-img")
	require.NoError(t, err)
	require.NotNil(t, inspected)
	require.Equal(t, ImageStateActive, inspected.State)
	require.Equal(t, "1.0", inspected.Version)
}

var _ = filepath.Join
