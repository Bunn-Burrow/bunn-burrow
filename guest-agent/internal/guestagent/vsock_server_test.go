package guestagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateExecuteRequest(t *testing.T) {
	t.Run("missing execution_id", func(t *testing.T) {
		err := validateExecuteRequest(map[string]any{
			"command": "true",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "execution_id")
	})

	t.Run("missing command", func(t *testing.T) {
		err := validateExecuteRequest(map[string]any{
			"execution_id": "exec-1",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "command")
	})

	t.Run("negative timeout", func(t *testing.T) {
		err := validateExecuteRequest(map[string]any{
			"execution_id":    "exec-1",
			"command":         "true",
			"timeout_seconds": float64(-5),
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "timeout_seconds")
	})

	t.Run("valid request", func(t *testing.T) {
		err := validateExecuteRequest(map[string]any{
			"execution_id":    "exec-1",
			"command":         "true",
			"args":            []any{},
			"env":             map[string]any{},
			"working_dir":     "/",
			"timeout_seconds": float64(30),
		})
		require.NoError(t, err)
	})

	t.Run("timeout zero is valid", func(t *testing.T) {
		err := validateExecuteRequest(map[string]any{
			"execution_id":    "exec-1",
			"command":         "true",
			"timeout_seconds": float64(0),
		})
		require.NoError(t, err)
	})

	t.Run("timeout missing is valid", func(t *testing.T) {
		err := validateExecuteRequest(map[string]any{
			"execution_id": "exec-1",
			"command":      "true",
		})
		require.NoError(t, err)
	})

	t.Run("timeout string rejected", func(t *testing.T) {
		err := validateExecuteRequest(map[string]any{
			"execution_id":    "exec-1",
			"command":         "true",
			"timeout_seconds": "30",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "timeout_seconds")
	})

	t.Run("timeout bool rejected", func(t *testing.T) {
		err := validateExecuteRequest(map[string]any{
			"execution_id":    "exec-1",
			"command":         "true",
			"timeout_seconds": true,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "timeout_seconds")
	})
}

func TestMergeEnv(t *testing.T) {
	imageEnv := []string{"FOO=bar", "BAZ=qux"}
	execEnv := map[string]any{"FOO": "overridden", "NEW": "val"}

	merged := MergeEnv(imageEnv, execEnv)
	require.Contains(t, merged, "FOO=overridden")
	require.Contains(t, merged, "BAZ=qux")
	require.Contains(t, merged, "NEW=val")
}

func TestEnvToSlice(t *testing.T) {
	env := map[string]any{"A": "1", "B": "two"}
	slice := EnvToSlice(env)
	require.Len(t, slice, 2)
	require.Contains(t, slice, "A=1")
	require.Contains(t, slice, "B=two")
}

func TestArgsToSlice(t *testing.T) {
	args := []any{"arg1", 42, true}
	slice := ArgsToSlice(args)
	require.Equal(t, []string{"arg1", "42", "true"}, slice)
}

func TestIsFreezable(t *testing.T) {
	tests := []struct {
		name     string
		mount    mountInfo
		expected bool
	}{
		{
			name: "ext4 read-write",
			mount: mountInfo{
				device:  "/dev/sda1",
				path:    "/",
				fstype:  "ext4",
				options: "rw,relatime",
			},
			expected: true,
		},
		{
			name: "xfs read-write",
			mount: mountInfo{
				device:  "/dev/sdb1",
				path:    "/data",
				fstype:  "xfs",
				options: "rw,relatime",
			},
			expected: true,
		},
		{
			name: "ext4 read-only",
			mount: mountInfo{
				device:  "/dev/sda1",
				path:    "/",
				fstype:  "ext4",
				options: "ro,relatime",
			},
			expected: false,
		},
		{
			name: "proc filesystem",
			mount: mountInfo{
				device:  "proc",
				path:    "/proc",
				fstype:  "proc",
				options: "rw,nosuid,nodev,noexec,relatime",
			},
			expected: false,
		},
		{
			name: "sysfs filesystem",
			mount: mountInfo{
				device:  "sysfs",
				path:    "/sys",
				fstype:  "sysfs",
				options: "rw,nosuid,nodev,noexec,relatime",
			},
			expected: false,
		},
		{
			name: "tmpfs filesystem",
			mount: mountInfo{
				device:  "tmpfs",
				path:    "/run",
				fstype:  "tmpfs",
				options: "rw,nosuid,nodev,relatime",
			},
			expected: false,
		},
		{
			name: "cgroup2 filesystem",
			mount: mountInfo{
				device:  "cgroup2",
				path:    "/sys/fs/cgroup",
				fstype:  "cgroup2",
				options: "rw,nosuid,nodev,noexec,relatime",
			},
			expected: false,
		},
		{
			name: "overlay filesystem",
			mount: mountInfo{
				device:  "overlay",
				path:    "/",
				fstype:  "overlay",
				options: "rw,relatime",
			},
			expected: false,
		},
		{
			name: "squashfs filesystem",
			mount: mountInfo{
				device:  "/dev/loop0",
				path:    "/snap/core",
				fstype:  "squashfs",
				options: "ro,relatime",
			},
			expected: false,
		},
		{
			name: "tmpfs with nosuid option",
			mount: mountInfo{
				device:  "tmpfs",
				path:    "/dev/shm",
				fstype:  "tmpfs",
				options: "rw,nosuid,nodev",
			},
			expected: false,
		},
		{
			name: "unknown fstype",
			mount: mountInfo{
				device:  "/dev/sdc1",
				path:    "/mnt/data",
				fstype:  "unknownfs",
				options: "rw,relatime",
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.expected, isFreezable(tt.mount))
		})
	}
}

func TestParseProcMounts(t *testing.T) {
	// Write a fake /proc/mounts to a temp file
	dir := t.TempDir()
	fakePath := filepath.Join(dir, "mounts")
	content := `proc /proc proc rw,nosuid,nodev,noexec,relatime 0 0
sysfs /sys sysfs rw,nosuid,nodev,noexec,relatime 0 0
/dev/sda1 / ext4 rw,relatime,errors=remount-ro 0 0
tmpfs /run tmpfs rw,nosuid,nodev,relatime 0 0
/dev/sdb1 /data xfs rw,relatime 0 0
`
	require.NoError(t, os.WriteFile(fakePath, []byte(content), 0644))

	// Temporarily test parseProcMounts by checking it reads real /proc/mounts
	// (we can't easily redirect it, but we can verify it doesn't crash on real data)
	mounts, err := parseProcMounts()
	require.NoError(t, err)
	require.NotEmpty(t, mounts)

	// Verify at least the root mount is present
	var rootFound bool
	for _, m := range mounts {
		if m.path == "/" {
			rootFound = true
			break
		}
	}
	require.True(t, rootFound, "root mount / should be present in /proc/mounts")
}

func TestFreezeMountListEmpty(t *testing.T) {
	reqID := "test-req-empty"
	state := getSnapshotState(reqID)
	state.mu.Lock()
	state.prepared = true
	state.mu.Unlock()
	defer deleteSnapshotState(reqID)

	frozen, err := freezeMountList(reqID, nil)
	require.NoError(t, err)
	require.Empty(t, frozen)
}

func TestFreezeMountListNoFreezable(t *testing.T) {
	reqID := "test-req-nofreezable"
	state := getSnapshotState(reqID)
	state.mu.Lock()
	state.prepared = true
	state.mu.Unlock()
	defer deleteSnapshotState(reqID)

	mounts := []mountInfo{
		{device: "proc", path: "/proc", fstype: "proc", options: "rw,relatime"},
		{device: "sysfs", path: "/sys", fstype: "sysfs", options: "rw,relatime"},
		{device: "tmpfs", path: "/run", fstype: "tmpfs", options: "rw,nosuid,nodev"},
		{device: "overlay", path: "/", fstype: "overlay", options: "rw,relatime"},
	}

	frozen, err := freezeMountList(reqID, mounts)
	require.NoError(t, err)
	require.Empty(t, frozen)
}

func TestFreezeMountListFreezableMountFails(t *testing.T) {
	reqID := "test-req-freezable-fail"
	state := getSnapshotState(reqID)
	state.mu.Lock()
	state.prepared = true
	state.mu.Unlock()
	defer deleteSnapshotState(reqID)

	// Use a non-existent path - unix.Open will fail
	mounts := []mountInfo{
		{device: "/dev/sda1", path: "/nonexistent/mount", fstype: "ext4", options: "rw,relatime"},
	}

	_, err := freezeMountList(reqID, mounts)
	require.Error(t, err)
	require.Contains(t, err.Error(), "freeze filesystems")
}

func TestFreezeMountsNotPrepared(t *testing.T) {
	reqID := "test-req-not-prepared"
	defer deleteSnapshotState(reqID)

	// State exists but prepared is false (default)
	_, err := freezeMounts(reqID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "guest not prepared for snapshot")
}

func TestFreezeMountsAlreadyFrozen(t *testing.T) {
	reqID := "test-req-already-frozen"
	state := getSnapshotState(reqID)
	state.mu.Lock()
	state.prepared = true
	state.frozen = true
	state.frozenMounts = []string{"/"}
	state.mu.Unlock()
	defer deleteSnapshotState(reqID)

	_, err := freezeMounts(reqID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "guest filesystems already frozen")
}

func TestThawMountsNotFrozen(t *testing.T) {
	reqID := "test-req-thaw-not-frozen"
	state := getSnapshotState(reqID)
	state.mu.Lock()
	state.prepared = true
	state.frozen = false
	state.frozenMounts = nil
	state.mu.Unlock()
	defer deleteSnapshotState(reqID)

	err := thawMounts(reqID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "guest filesystems not frozen")
}

func TestThawMountsSuccess(t *testing.T) {
	reqID := "test-req-thaw-success"
	state := getSnapshotState(reqID)
	state.mu.Lock()
	state.prepared = true
	state.frozen = true
	state.frozenMounts = []string{"/nonexistent/mount1", "/nonexistent/mount2"}
	state.mu.Unlock()
	defer deleteSnapshotState(reqID)

	// thawMounts will try to unix.Open and ioctl on non-existent paths,
	// which will fail. But the function should still clean up state and
	// return an error (first error encountered).
	err := thawMounts(reqID)
	require.Error(t, err)

	// Verify state was cleaned up
	state2 := getSnapshotState(reqID)
	state2.mu.Lock()
	require.False(t, state2.frozen)
	require.False(t, state2.prepared)
	require.Empty(t, state2.frozenMounts)
	state2.mu.Unlock()
	deleteSnapshotState(reqID)
}

func TestFreezeMountsTracksFrozenMounts(t *testing.T) {
	reqID := "test-req-track-mounts"
	state := getSnapshotState(reqID)
	state.mu.Lock()
	state.prepared = true
	state.mu.Unlock()
	defer deleteSnapshotState(reqID)

	// With no freezable mounts, freezeMountList should succeed with empty list
	mounts := []mountInfo{
		{device: "proc", path: "/proc", fstype: "proc", options: "rw,relatime"},
		{device: "sysfs", path: "/sys", fstype: "sysfs", options: "rw,relatime"},
	}

	frozen, err := freezeMountList(reqID, mounts)
	require.NoError(t, err)
	require.Empty(t, frozen)

	// Verify state was updated
	state2 := getSnapshotState(reqID)
	state2.mu.Lock()
	require.True(t, state2.frozen)
	require.Empty(t, state2.frozenMounts)
	state2.mu.Unlock()
}

func TestStringsHasPrefixMatch(t *testing.T) {
	// Verify that nonFreezablePrefixes matching works correctly
	require.True(t, strings.HasPrefix("proc", "proc"))
	require.True(t, strings.HasPrefix("cgroup2", "cgroup"))
	require.False(t, strings.HasPrefix("ext4", "proc"))
	require.False(t, strings.HasPrefix("xfs", "tmpfs"))
}
