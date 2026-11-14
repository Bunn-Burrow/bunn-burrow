package rootfs

import (
	"archive/tar"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

type layerEntry struct {
	name     string
	typeflag byte
	linkname string
	mode     int64
	data     string
}

func writeLayer(t *testing.T, entries ...layerEntry) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "layer.tar")
	file, err := os.Create(path)
	require.NoError(t, err)
	writer := tar.NewWriter(file)
	for _, entry := range entries {
		mode := entry.mode
		if mode == 0 {
			mode = 0o644
		}
		require.NoError(t, writer.WriteHeader(&tar.Header{
			Name:     entry.name,
			Typeflag: entry.typeflag,
			Linkname: entry.linkname,
			Mode:     mode,
			Size:     int64(len(entry.data)),
		}))
		if entry.data != "" {
			_, err := writer.Write([]byte(entry.data))
			require.NoError(t, err)
		}
	}
	require.NoError(t, writer.Close())
	require.NoError(t, file.Close())
	return path
}

func TestApplyLayerOpaqueWhiteoutRemovesDirectoryContents(t *testing.T) {
	rootDir := t.TempDir()
	builder := New(rootDir)
	require.NoError(t, builder.PrepareDir())

	first := writeLayer(t,
		layerEntry{name: "opt/app", typeflag: tar.TypeDir, mode: 0o755},
		layerEntry{name: "opt/app/old", typeflag: tar.TypeReg, data: "old"},
		layerEntry{name: "opt/app/sub", typeflag: tar.TypeDir, mode: 0o755},
		layerEntry{name: "opt/app/sub/old", typeflag: tar.TypeReg, data: "old"},
	)
	require.NoError(t, builder.ApplyLayer(first))

	second := writeLayer(t,
		layerEntry{name: "opt/app/.wh..wh..opq", typeflag: tar.TypeReg},
		layerEntry{name: "opt/app/new", typeflag: tar.TypeReg, data: "new"},
	)
	require.NoError(t, builder.ApplyLayer(second))

	_, err := os.Stat(filepath.Join(rootDir, "opt/app/old"))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(filepath.Join(rootDir, "opt/app/sub"))
	require.ErrorIs(t, err, os.ErrNotExist)
	data, err := os.ReadFile(filepath.Join(rootDir, "opt/app/new"))
	require.NoError(t, err)
	require.Equal(t, "new", string(data))
}

func TestApplyLayerRejectsTraversalPath(t *testing.T) {
	rootDir := t.TempDir()
	builder := New(rootDir)
	require.NoError(t, builder.PrepareDir())
	layer := writeLayer(t, layerEntry{name: "../../outside", typeflag: tar.TypeReg, data: "unsafe"})

	err := builder.ApplyLayer(layer)
	require.ErrorIs(t, err, ErrInvalidPath)
	_, err = os.Stat(filepath.Join(filepath.Dir(rootDir), "outside"))
	require.True(t, errors.Is(err, os.ErrNotExist))
}

func TestApplyLayerWhiteoutRemovesTarget(t *testing.T) {
	rootDir := t.TempDir()
	builder := New(rootDir)
	require.NoError(t, builder.PrepareDir())

	first := writeLayer(t, layerEntry{name: "app/old", typeflag: tar.TypeReg, data: "old"})
	require.NoError(t, builder.ApplyLayer(first))
	second := writeLayer(t, layerEntry{name: "app/.wh.old", typeflag: tar.TypeReg})
	require.NoError(t, builder.ApplyLayer(second))

	_, err := os.Stat(filepath.Join(rootDir, "app/old"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestApplyLayerResolvesGuestSymlinksInsideRootfs(t *testing.T) {
	rootDir := t.TempDir()
	builder := New(rootDir)
	require.NoError(t, builder.PrepareDir())
	layer := writeLayer(t,
		layerEntry{name: "absolute", typeflag: tar.TypeSymlink, linkname: "/safe"},
		layerEntry{name: "absolute/file", typeflag: tar.TypeReg, data: "absolute"},
		layerEntry{name: "relative", typeflag: tar.TypeSymlink, linkname: "../../relative-target"},
		layerEntry{name: "relative/file", typeflag: tar.TypeReg, data: "relative"},
	)

	require.NoError(t, builder.ApplyLayer(layer))
	data, err := os.ReadFile(filepath.Join(rootDir, "safe/file"))
	require.NoError(t, err)
	require.Equal(t, "absolute", string(data))
	data, err = os.ReadFile(filepath.Join(rootDir, "relative-target/file"))
	require.NoError(t, err)
	require.Equal(t, "relative", string(data))
	_, err = os.Stat(filepath.Join(filepath.Dir(rootDir), "relative-target"))
	require.True(t, errors.Is(err, os.ErrNotExist))
}

func TestApplyLayerReplacesFinalSymlinkWithRegularFile(t *testing.T) {
	rootDir := t.TempDir()
	builder := New(rootDir)
	require.NoError(t, builder.PrepareDir())

	first := writeLayer(t,
		layerEntry{name: "target", typeflag: tar.TypeReg, data: "target"},
		layerEntry{name: "entry", typeflag: tar.TypeSymlink, linkname: "target"},
	)
	require.NoError(t, builder.ApplyLayer(first))
	second := writeLayer(t, layerEntry{name: "entry", typeflag: tar.TypeReg, data: "replacement"})
	require.NoError(t, builder.ApplyLayer(second))

	entry, err := os.Lstat(filepath.Join(rootDir, "entry"))
	require.NoError(t, err)
	require.Zero(t, entry.Mode()&os.ModeSymlink)
	data, err := os.ReadFile(filepath.Join(rootDir, "entry"))
	require.NoError(t, err)
	require.Equal(t, "replacement", string(data))
	data, err = os.ReadFile(filepath.Join(rootDir, "target"))
	require.NoError(t, err)
	require.Equal(t, "target", string(data))
}

func TestFinalizeDefersDirectoryPermissionsAcrossLayers(t *testing.T) {
	rootDir := t.TempDir()
	builder := New(rootDir)
	require.NoError(t, builder.PrepareDir())

	first := writeLayer(t, layerEntry{name: "locked", typeflag: tar.TypeDir, mode: 0o555})
	require.NoError(t, builder.ApplyLayer(first))
	second := writeLayer(t, layerEntry{name: "locked/file", typeflag: tar.TypeReg, data: "ok"})
	require.NoError(t, builder.ApplyLayer(second))
	require.NoError(t, builder.Finalize())

	info, err := os.Stat(filepath.Join(rootDir, "locked"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o555), info.Mode().Perm())
	require.NoError(t, os.Chmod(filepath.Join(rootDir, "locked"), 0o755))
}

func TestFinalizeDoesNotApplyDirectoryModeAfterReplacement(t *testing.T) {
	rootDir := t.TempDir()
	builder := New(rootDir)
	require.NoError(t, builder.PrepareDir())

	first := writeLayer(t, layerEntry{name: "entry", typeflag: tar.TypeDir, mode: 0o555})
	require.NoError(t, builder.ApplyLayer(first))
	second := writeLayer(t, layerEntry{name: "entry", typeflag: tar.TypeReg, data: "file"})
	require.NoError(t, builder.ApplyLayer(second))
	require.NoError(t, builder.Finalize())

	info, err := os.Stat(filepath.Join(rootDir, "entry"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o644), info.Mode().Perm())
}
