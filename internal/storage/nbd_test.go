package storage

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestListNBDDevicesValidNames(t *testing.T) {
	dir := t.TempDir()

	// Create valid NBD device nodes.
	for _, name := range []string{"nbd0", "nbd1", "nbd10", "nbd255"} {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, name), 0o755))
	}

	// Create invalid entries that should be ignored.
	for _, name := range []string{"nbd", "nbd0p1", "nbdabc", "nbd-1", "xyz", "nbd0x"} {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, name), 0o755))
	}

	records, err := ListNBDDevices(dir, "/var/lock")
	require.NoError(t, err)

	// Expect exactly 4 valid devices: nbd0, nbd1, nbd10, nbd255.
	require.Len(t, records, 4)

	nums := map[int]bool{}
	for _, r := range records {
		nums[r.Number] = true
		require.Equal(t, "nbd"+strconv.Itoa(r.Number), filepath.Base(r.Device))
		require.Equal(t, dir, filepath.Dir(r.Device))
		require.Equal(t, filepath.Join("/var/lock", "qemu-nbd-nbd"+strconv.Itoa(r.Number)), r.LockPath)
	}
	require.True(t, nums[0])
	require.True(t, nums[1])
	require.True(t, nums[10])
	require.True(t, nums[255])
}

func TestListNBDDevicesEmptyDir(t *testing.T) {
	dir := t.TempDir()
	records, err := ListNBDDevices(dir, "/var/lock")
	require.NoError(t, err)
	require.Empty(t, records)
}

func TestListNBDDevicesLockDir(t *testing.T) {
	dir := t.TempDir()
	lockDir := t.TempDir()

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "nbd3"), 0o755))

	records, err := ListNBDDevices(dir, lockDir)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, filepath.Join(lockDir, "qemu-nbd-nbd3"), records[0].LockPath)
}

func TestProcessIsQemuNBD(t *testing.T) {
	// Non-existent PID should return false.
	require.False(t, ProcessIsQemuNBD(999999))

	// Current process is not qemu-nbd.
	require.False(t, ProcessIsQemuNBD(os.Getpid()))
}
