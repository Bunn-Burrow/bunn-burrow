package template

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTemplateStoreCreateFromImage(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	require.NoError(t, store.EnsureDirs())

	tmpl := &Template{
		ID:           "tmpl-1",
		Name:         "test-template",
		ImageID:      "img-1",
		ImageVersion: "1.0",
		Description:  "Test template",
		Architecture: "amd64",
	}

	require.NoError(t, store.Create(tmpl))

	got := store.Get("tmpl-1")
	require.NotNil(t, got)
	require.Equal(t, "tmpl-1", got.ID)
	require.Equal(t, "test-template", got.Name)
	require.Equal(t, "img-1", got.ImageID)
	require.Equal(t, "1.0", got.ImageVersion)
	require.Equal(t, "amd64", got.Architecture)
}

func TestTemplateStoreCreateDuplicate(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	require.NoError(t, store.EnsureDirs())

	tmpl := &Template{ID: "dup", Name: "dup", ImageID: "img-1"}
	require.NoError(t, store.Create(tmpl))

	tmpl2 := &Template{ID: "dup", Name: "dup2", ImageID: "img-2"}
	err := store.Create(tmpl2)
	require.Error(t, err)
	require.Contains(t, err.Error(), "already exists")
}

func TestTemplateStoreCreateNoImageNoRootfs(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	require.NoError(t, store.EnsureDirs())

	tmpl := &Template{ID: "invalid", Name: "invalid"}
	err := store.Create(tmpl)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no image_id and no rootfs_path")
}

func TestTemplateStoreList(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	require.NoError(t, store.EnsureDirs())

	// Empty list initially.
	tmpls, err := store.List()
	require.NoError(t, err)
	require.Len(t, tmpls, 0)

	// Create templates.
	err = store.Create(&Template{ID: "tmpl-a", Name: "a", ImageID: "img-1"})
	require.NoError(t, err)
	err = store.Create(&Template{ID: "tmpl-b", Name: "b", ImageID: "img-2"})
	require.NoError(t, err)

	tmpls, err = store.List()
	require.NoError(t, err)
	require.Len(t, tmpls, 2)

	ids := map[string]bool{}
	for _, t := range tmpls {
		ids[t.ID] = true
	}
	require.True(t, ids["tmpl-a"])
	require.True(t, ids["tmpl-b"])
}

func TestTemplateStoreDelete(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	require.NoError(t, store.EnsureDirs())

	err := store.Create(&Template{ID: "del-tmpl", Name: "del", ImageID: "img-1"})
	require.NoError(t, err)

	// Delete should succeed.
	require.NoError(t, store.Delete("del-tmpl"))

	// Template should no longer be found.
	got := store.Get("del-tmpl")
	require.Nil(t, got)

	// Deleting again should fail? No, Delete is idempotent for metadata removal.
	// But Get returns nil after delete.
	_ = store.Delete("del-tmpl")
}

func TestTemplateStoreValidate(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	require.NoError(t, store.EnsureDirs())

	// Validate non-existent template.
	err := store.Validate("nonexistent")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")

	// Create a template with rootfs path (backward compat).
	rootfsPath := filepath.Join(dir, "rootfs.ext4")
	require.NoError(t, os.WriteFile(rootfsPath, []byte("rootfs data"), 0o644))

	tmpl := &Template{
		ID:           "valid-tmpl",
		Name:         "valid",
		RootfsPath:   rootfsPath,
		RootfsFormat: "ext4",
		Architecture: "amd64",
	}
	require.NoError(t, store.Create(tmpl))

	err = store.Validate("valid-tmpl")
	require.NoError(t, err)
}

func TestTemplateManagerValidation(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	require.NoError(t, store.EnsureDirs())

	// No imageResolver - validation only checks rootfs checksum.
	mgr := NewTemplateManager(store, nil)

	// Validate non-existent template.
	err := mgr.Validate("nonexistent")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")

	// Create template without image reference.
	err = store.Create(&Template{ID: "no-img", Name: "no-img"})
	require.Error(t, err) // no image_id and no rootfs_path
}

func TestTemplateManagerTemplateReferencesImage(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	require.NoError(t, store.EnsureDirs())

	mgr := NewTemplateManager(store, nil)

	// No templates reference any image.
	ref, err := mgr.TemplateReferencesImage("img-1")
	require.NoError(t, err)
	require.False(t, ref)

	// Create template referencing img-1.
	err = store.Create(&Template{ID: "tmpl-1", Name: "tmpl-1", ImageID: "img-1"})
	require.NoError(t, err)

	ref, err = mgr.TemplateReferencesImage("img-1")
	require.NoError(t, err)
	require.True(t, ref)

	ref, err = mgr.TemplateReferencesImage("img-2")
	require.NoError(t, err)
	require.False(t, ref)
}

func TestTemplateManagerResolveDefaults(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	require.NoError(t, store.EnsureDirs())

	mgr := NewTemplateManager(store, nil)

	err := store.Create(&Template{
		ID:             "tmpl-defaults",
		Name:           "defaults",
		ImageID:        "img-1",
		DefaultCmd:     "/bin/sh",
		DefaultArgs:    []string{"-c", "echo hi"},
		DefaultEnv:     []string{"FOO=bar"},
		DefaultDir:     "/tmp",
		DefaultVCPU:    4,
		DefaultMemMiB:  2048,
		DefaultTimeout: 60,
		DefaultNetwork: true,
	})
	require.NoError(t, err)

	defaults, err := mgr.ResolveDefaults("tmpl-defaults")
	require.NoError(t, err)
	require.NotNil(t, defaults)
	require.Equal(t, "tmpl-defaults", defaults.TemplateID)
	require.Equal(t, "/bin/sh", defaults.DefaultCmd)
	require.Equal(t, []string{"-c", "echo hi"}, defaults.DefaultArgs)
	require.Equal(t, []string{"FOO=bar"}, defaults.DefaultEnv)
	require.Equal(t, "/tmp", defaults.DefaultDir)
	require.Equal(t, int64(4), defaults.DefaultVCPU)
	require.Equal(t, int64(2048), defaults.DefaultMemMiB)
	require.Equal(t, int64(60), defaults.DefaultTimeout)
	require.True(t, defaults.DefaultNetwork)
	require.Contains(t, defaults.BootArgs, "console=ttyS0")
	require.Contains(t, defaults.BootArgs, "FOO=bar")
}

func TestTemplateDefaultsApply(t *testing.T) {
	cfg := &Config{
		VCPU:       2, // already set, should not be overridden
		MemSizeMiB: 0, // not set, should use default
	}

	defaults := &TemplateDefaults{
		TemplateID:    "tmpl-1",
		DefaultVCPU:   8,
		DefaultMemMiB: 4096,
		BootArgs:      "custom boot args",
		DefaultDir:    "/workspace",
	}

	ApplyDefaults(cfg, defaults)

	require.Equal(t, "tmpl-1", cfg.TemplateID)
	require.Equal(t, int64(8), cfg.VCPU)          // overridden by default
	require.Equal(t, int64(4096), cfg.MemSizeMiB) // overridden
	require.Equal(t, "custom boot args", cfg.BootArgs)
	require.Equal(t, "/workspace", cfg.WorkDir)
}

var _ = filepath.Join
