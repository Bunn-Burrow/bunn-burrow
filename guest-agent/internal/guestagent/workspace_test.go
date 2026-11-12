package guestagent

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateProjectPath(t *testing.T) {
	limits := DefaultWorkspaceLimits()

	t.Run("valid relative path", func(t *testing.T) {
		require.NoError(t, validateProjectPath("main.go", limits))
		require.NoError(t, validateProjectPath("dir/subdir/file.go", limits))
		require.NoError(t, validateProjectPath("config/app.yaml", limits))
	})

	t.Run("empty path rejected", func(t *testing.T) {
		require.Error(t, validateProjectPath("", limits))
	})

	t.Run("absolute path rejected", func(t *testing.T) {
		require.Error(t, validateProjectPath("/etc/passwd", limits))
		require.Error(t, validateProjectPath("/tmp/foo", limits))
	})

	t.Run("dot-dot traversal rejected", func(t *testing.T) {
		require.Error(t, validateProjectPath("../etc/passwd", limits))
		require.Error(t, validateProjectPath("dir/../../etc/passwd", limits))
		require.Error(t, validateProjectPath("..", limits))
	})

	t.Run("path too long rejected", func(t *testing.T) {
		longPath := strings.Repeat("a/", 2000) + "file.txt"
		require.Error(t, validateProjectPath(longPath, limits))
	})

	t.Run("excessive depth rejected", func(t *testing.T) {
		deepPath := strings.Repeat("dir"+string(filepath.Separator), limits.MaxDepth+1) + "file.txt"
		require.Error(t, validateProjectPath(deepPath, limits))
	})
}

func TestExtractTarValid(t *testing.T) {
	dir := t.TempDir()

	// Create a simple tar archive in memory.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	files := map[string]string{
		"main.go":                `package main\nfunc main() {}`,
		"internal/app/server.go": `package app\nfunc Start() {}`,
		"config/app.yaml":        `port: 8080\nhost: localhost`,
		"empty.txt":              ``,
		"nested/dir/readme.md":   `# Project`,
	}
	for name, content := range files {
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o640,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}
		require.NoError(t, tw.WriteHeader(hdr))
		_, err := tw.Write([]byte(content))
		require.NoError(t, err)
	}
	// Add a directory entry.
	dirHdr := &tar.Header{
		Name:     "nested/dir",
		Mode:     0o750,
		Typeflag: tar.TypeDir,
	}
	require.NoError(t, tw.WriteHeader(dirHdr))
	require.NoError(t, tw.Close())

	require.NoError(t, extractTar(bytes.NewReader(buf.Bytes()), dir, DefaultWorkspaceLimits()))

	// Verify files were created.
	for name, content := range files {
		fullPath := filepath.Join(dir, name)
		data, err := os.ReadFile(fullPath)
		require.NoError(t, err)
		require.Equal(t, content, string(data), "file %s content mismatch", name)
	}
}

func TestExtractTarRejectsSymlink(t *testing.T) {
	dir := t.TempDir()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	// Symlink pointing outside workspace.
	hdr := &tar.Header{
		Name:     "escape",
		Mode:     0o777,
		Linkname: "/etc/passwd",
		Typeflag: tar.TypeSymlink,
	}
	require.NoError(t, tw.WriteHeader(hdr))
	require.NoError(t, tw.Close())

	err := extractTar(bytes.NewReader(buf.Bytes()), dir, DefaultWorkspaceLimits())
	require.Error(t, err)
	require.Contains(t, err.Error(), "symlink/hardlink")
}

func TestExtractTarRejectsHardlink(t *testing.T) {
	dir := t.TempDir()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	hdr := &tar.Header{
		Name:     "escape",
		Mode:     0o777,
		Linkname: "/etc/passwd",
		Typeflag: tar.TypeLink,
	}
	require.NoError(t, tw.WriteHeader(hdr))
	require.NoError(t, tw.Close())

	err := extractTar(bytes.NewReader(buf.Bytes()), dir, DefaultWorkspaceLimits())
	require.Error(t, err)
	require.Contains(t, err.Error(), "symlink/hardlink")
}

func TestExtractTarRejectsAbsolutePath(t *testing.T) {
	dir := t.TempDir()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	hdr := &tar.Header{
		Name:     "/etc/passwd",
		Mode:     0o640,
		Size:     10,
		Typeflag: tar.TypeReg,
	}
	require.NoError(t, tw.WriteHeader(hdr))
	_, err := tw.Write([]byte("0123456789"))
	require.NoError(t, err)
	require.NoError(t, tw.Close())

	err = extractTar(bytes.NewReader(buf.Bytes()), dir, DefaultWorkspaceLimits())
	require.Error(t, err)
	require.Contains(t, err.Error(), "absolute path")
}

func TestExtractTarRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	hdr := &tar.Header{
		Name:     "../etc/passwd",
		Mode:     0o640,
		Size:     10,
		Typeflag: tar.TypeReg,
	}
	require.NoError(t, tw.WriteHeader(hdr))
	_, err := tw.Write([]byte("0123456789"))
	require.NoError(t, err)
	require.NoError(t, tw.Close())

	err = extractTar(bytes.NewReader(buf.Bytes()), dir, DefaultWorkspaceLimits())
	require.Error(t, err)
	require.Contains(t, err.Error(), "traversal")
}

func TestExtractTarRejectsOversizedFile(t *testing.T) {
	dir := t.TempDir()
	limits := DefaultWorkspaceLimits()
	limits.MaxFileSize = 100 // very small

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	hdr := &tar.Header{
		Name:     "big.txt",
		Mode:     0o640,
		Size:     200,
		Typeflag: tar.TypeReg,
	}
	require.NoError(t, tw.WriteHeader(hdr))
	_, err := tw.Write(bytes.Repeat([]byte("x"), 200))
	require.NoError(t, err)
	require.NoError(t, tw.Close())

	err = extractTar(bytes.NewReader(buf.Bytes()), dir, limits)
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds maximum size")
}

func TestReadFileFromWorkspace(t *testing.T) {
	dir := t.TempDir()

	// Create a file in the workspace.
	testContent := "hello world"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello.txt"), []byte(testContent), 0o640))

	data, err := readFileFromWorkspace(dir, "hello.txt", DefaultWorkspaceLimits())
	require.NoError(t, err)
	require.Equal(t, testContent, string(data))

	// Missing file.
	_, err = readFileFromWorkspace(dir, "missing.txt", DefaultWorkspaceLimits())
	require.Error(t, err)

	// Absolute path rejected.
	_, err = readFileFromWorkspace(dir, "/etc/passwd", DefaultWorkspaceLimits())
	require.Error(t, err)

	// Path traversal rejected.
	_, err = readFileFromWorkspace(dir, "../secret.txt", DefaultWorkspaceLimits())
	require.Error(t, err)
}

func TestReadFileFromWorkspaceRejectsEscape(t *testing.T) {
	dir := t.TempDir()

	// Create a symlink inside the workspace pointing outside.
	target := filepath.Join(t.TempDir(), "outside.txt")
	require.NoError(t, os.WriteFile(target, []byte("secret"), 0o640))
	linkPath := filepath.Join(dir, "escape.txt")
	require.NoError(t, os.Symlink(target, linkPath))

	// EvalSymlinks would resolve outside the workspace.
	_, err := readFileFromWorkspace(dir, "escape.txt", DefaultWorkspaceLimits())
	require.Error(t, err)
	require.Contains(t, err.Error(), "escapes workspace")
}

var _ = strings.TrimSpace
