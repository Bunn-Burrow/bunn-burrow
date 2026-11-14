package prepare

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildExt4ImagePreservesExecutableFilesAndSymlinks(t *testing.T) {
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 is not installed")
	}
	if _, err := exec.LookPath("debugfs"); err != nil {
		t.Skip("debugfs is not installed")
	}

	root := t.TempDir()
	binDir := filepath.Join(root, "usr", "local", "bin")
	require.NoError(t, os.MkdirAll(binDir, 0o755))
	executable := filepath.Join(binDir, "tool")
	require.NoError(t, os.WriteFile(executable, []byte("payload"), 0o755))
	require.NoError(t, os.Chmod(executable, 0o755))
	require.NoError(t, os.Symlink("/usr/local/bin/tool", filepath.Join(root, "tool-link")))
	require.NoError(t, os.WriteFile(filepath.Join(root, "name with spaces;"), []byte("safe"), 0o644))

	imagePath := filepath.Join(t.TempDir(), "rootfs.ext4")
	require.NoError(t, buildExt4Image(root, imagePath))

	statOutput, err := exec.Command("debugfs", "-R", "stat /usr/local/bin/tool", imagePath).CombinedOutput()
	require.NoError(t, err, string(statOutput))
	require.Contains(t, string(statOutput), "Mode:  0755")

	symlinkOutput, err := exec.Command("debugfs", "-R", "stat /tool-link", imagePath).CombinedOutput()
	require.NoError(t, err, string(symlinkOutput))
	require.Contains(t, string(symlinkOutput), "Fast link dest: \"/usr/local/bin/tool\"")

	fileOutput, err := exec.Command("debugfs", "-R", "ls -l /", imagePath).CombinedOutput()
	require.NoError(t, err, string(fileOutput))
	require.Contains(t, string(fileOutput), "name with spaces;")
}
