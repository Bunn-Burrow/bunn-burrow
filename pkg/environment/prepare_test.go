package environment

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Bunn-Burrow/bunn-burrow/vmm"
	"github.com/stretchr/testify/require"
)

func TestResolveImageReference(t *testing.T) {
	tests := map[string]string{
		"bunn/ubuntu:24.04":     "ubuntu:24.04",
		"bunn/alpine:latest":    "alpine:3.24",
		"bunn/python:3.12":      "python:3.12-alpine",
		"bunn/node:20":          "node:20-alpine",
		"bunn/go:1.26":          "golang:1.26-alpine",
		"bunn/fullstack":        "ubuntu:24.04",
		"bunn/fullstack:latest": "ubuntu:24.04",
		"alpine:3.24":           "alpine:3.24",
	}
	for input, expected := range tests {
		t.Run(input, func(t *testing.T) {
			got, err := ResolveImageReference(input)
			require.NoError(t, err)
			require.Equal(t, expected, got)
		})
	}
}

func TestResolveImageReferenceRejectsEmpty(t *testing.T) {
	_, err := ResolveImageReference("")
	require.ErrorContains(t, err, "required")
}

func TestGuestAgentVersionUsesContentHash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "guest-agent")
	require.NoError(t, os.WriteFile(path, []byte("first"), 0o600))
	first, err := GuestAgentVersion(path)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(path, []byte("second"), 0o600))
	second, err := GuestAgentVersion(path)
	require.NoError(t, err)
	require.NotEqual(t, first, second)
	require.Contains(t, first, "sha256:")
}

func TestBuildFullstackUsesCachedProfileWithoutNetwork(t *testing.T) {
	cacheDir := t.TempDir()
	base := &Prepared{CacheKey: "ubuntu-cache-key", RootfsPath: filepath.Join(t.TempDir(), "ubuntu.ext4")}
	require.NoError(t, os.WriteFile(base.RootfsPath, []byte("base"), 0o600))

	profilePath := filepath.Join(cacheDir, "profiles", fullstackProfileKey(base.CacheKey), "rootfs.ext4")
	require.NoError(t, os.MkdirAll(filepath.Dir(profilePath), 0o750))
	require.NoError(t, os.WriteFile(profilePath, []byte("profile"), 0o600))

	preparer := &Preparer{cacheDir: cacheDir}
	profile, err := preparer.BuildFullstack(context.Background(), base, vmm.Config{}, nil)
	require.NoError(t, err)
	require.Equal(t, profilePath, profile.RootfsPath)
	require.Equal(t, "bunn/fullstack:latest", profile.ImageReference)

	baseData, err := os.ReadFile(base.RootfsPath)
	require.NoError(t, err)
	require.Equal(t, []byte("base"), baseData)
}

func TestBuildFullstackRequiresNetworkOnCacheMiss(t *testing.T) {
	base := &Prepared{CacheKey: "ubuntu-cache-key", RootfsPath: filepath.Join(t.TempDir(), "ubuntu.ext4")}
	require.NoError(t, os.WriteFile(base.RootfsPath, []byte("base"), 0o600))

	preparer := &Preparer{cacheDir: t.TempDir()}
	_, err := preparer.BuildFullstack(context.Background(), base, vmm.Config{}, nil)
	require.ErrorContains(t, err, "requires guest networking")
}
