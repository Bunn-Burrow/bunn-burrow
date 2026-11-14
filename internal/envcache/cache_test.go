package envcache

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGetRecoversRelocatedRootfsPath(t *testing.T) {
	cacheDir := t.TempDir()
	cache := New(filepath.Join(cacheDir, "environments"))
	require.NoError(t, cache.EnsureDir())

	key := "sha256:abc123:linux/amd64:agent-v1"
	rootfsPath := filepath.Join(cacheDir, "base", safeCacheKey(key), "rootfs.ext4")
	require.NoError(t, os.MkdirAll(filepath.Dir(rootfsPath), 0o750))
	require.NoError(t, os.WriteFile(rootfsPath, []byte("ext4"), 0o600))

	env := &Environment{
		CacheKey:   key,
		RootfsPath: filepath.Join(t.TempDir(), "old-checkout", "rootfs.ext4"),
	}
	require.NoError(t, cache.Store(env))

	got := cache.Get(key)
	require.NotNil(t, got)
	require.Equal(t, rootfsPath, got.RootfsPath)
}
