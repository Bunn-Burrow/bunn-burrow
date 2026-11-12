package guestagent

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// WorkspaceLimits defines configurable limits for project uploads.
type WorkspaceLimits struct {
	// MaxTotalSize is the maximum total size of all files in a project.
	MaxTotalSize int64

	// MaxFileSize is the maximum size of a single file.
	MaxFileSize int64

	// MaxFiles is the maximum number of files in a project.
	MaxFiles int

	// MaxPathLength is the maximum length of a file path (including directories).
	MaxPathLength int

	// MaxDepth is the maximum directory nesting depth.
	MaxDepth int
}

// DefaultWorkspaceLimits returns sensible default limits for project uploads.
func DefaultWorkspaceLimits() WorkspaceLimits {
	return WorkspaceLimits{
		MaxTotalSize:  100 * 1024 * 1024, // 100 MiB
		MaxFileSize:   10 * 1024 * 1024,  // 10 MiB
		MaxFiles:      1000,
		MaxPathLength: 4096,
		MaxDepth:      32,
	}
}

// validateProjectPath checks that a relative path is safe to write inside a
// workspace. It rejects:
//   - Absolute paths
//   - Path traversal via ".."
//   - Paths that escape the workspace root
//   - Empty paths
//   - Paths exceeding max length or depth
func validateProjectPath(relPath string, limits WorkspaceLimits) error {
	if relPath == "" {
		return errors.New("empty path")
	}

	if len(relPath) > limits.MaxPathLength {
		return fmt.Errorf("path %q exceeds maximum length of %d", relPath, limits.MaxPathLength)
	}

	if filepath.IsAbs(relPath) {
		return fmt.Errorf("absolute path not allowed: %q", relPath)
	}

	// Clean the path and check for traversal.
	cleaned := filepath.Clean(relPath)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path traversal not allowed: %q", relPath)
	}

	// Reject paths containing ".." as any element.
	for _, elem := range strings.Split(cleaned, string(filepath.Separator)) {
		if elem == ".." {
			return fmt.Errorf("path traversal not allowed: %q", relPath)
		}
	}

	// Count directory depth (number of separators + 1 for the file itself).
	depth := strings.Count(cleaned, string(filepath.Separator)) + 1
	if depth > limits.MaxDepth {
		return fmt.Errorf("path %q exceeds maximum depth of %d", relPath, limits.MaxDepth)
	}

	return nil
}

// extractTar safely extracts a tar archive into the given directory, enforcing
// workspace limits and safety checks.
func extractTar(r io.Reader, destDir string, limits WorkspaceLimits) error {
	tr := tar.NewReader(r)

	var totalSize int64
	var fileCount int

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar read: %w", err)
		}

		// Validate the path.
		if err := validateProjectPath(hdr.Name, limits); err != nil {
			return fmt.Errorf("invalid entry %q: %w", hdr.Name, err)
		}

		// Reject symlinks and hardlinks (prevent escape attacks).
		switch hdr.Typeflag {
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf("symlink/hardlink entries not allowed: %q", hdr.Name)
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			return fmt.Errorf("special file type not allowed: %q", hdr.Name)
		}

		// Only regular files and directories are allowed.
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA && hdr.Typeflag != tar.TypeDir {
			return fmt.Errorf("unsupported entry type %d for %q", hdr.Typeflag, hdr.Name)
		}

		// Enforce per-file size limit.
		if hdr.Size > limits.MaxFileSize {
			return fmt.Errorf("file %q exceeds maximum size of %d bytes", hdr.Name, limits.MaxFileSize)
		}

		// Enforce total size limit.
		totalSize += hdr.Size
		if totalSize > limits.MaxTotalSize {
			return fmt.Errorf("project exceeds maximum total size of %d bytes", limits.MaxTotalSize)
		}

		// Enforce file count limit.
		if hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeRegA {
			fileCount++
			if fileCount > limits.MaxFiles {
				return fmt.Errorf("project exceeds maximum file count of %d", limits.MaxFiles)
			}
		}

		destPath := filepath.Join(destDir, hdr.Name)

		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(destPath, hdr.FileInfo().Mode().Perm()); err != nil {
				return fmt.Errorf("create directory %q: %w", destPath, err)
			}
			continue
		}

		// Create parent directories.
		if err := os.MkdirAll(filepath.Dir(destPath), 0o750); err != nil {
			return fmt.Errorf("create parent directory for %q: %w", destPath, err)
		}

		// Write the file.
		f, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, hdr.FileInfo().Mode().Perm())
		if err != nil {
			return fmt.Errorf("create file %q: %w", destPath, err)
		}

		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return fmt.Errorf("write file %q: %w", destPath, err)
		}

		if err := f.Close(); err != nil {
			return fmt.Errorf("close file %q: %w", destPath, err)
		}
	}

	return nil
}

// readFileFromWorkspace reads a file from the workspace directory, validating
// that the path stays within the workspace.
func readFileFromWorkspace(destDir, relPath string, limits WorkspaceLimits) ([]byte, error) {
	if err := validateProjectPath(relPath, limits); err != nil {
		return nil, err
	}

	fullPath := filepath.Join(destDir, relPath)

	// Verify the resolved path is still within the workspace (defense in depth).
	resolved, err := filepath.EvalSymlinks(fullPath)
	if err != nil {
		// If the file doesn't exist yet, EvalSymlinks fails; use the joined path.
		resolved = fullPath
	}
	workspaceResolved, err := filepath.EvalSymlinks(destDir)
	if err != nil {
		workspaceResolved = destDir
	}
	if !strings.HasPrefix(resolved, workspaceResolved+string(filepath.Separator)) && resolved != workspaceResolved {
		return nil, fmt.Errorf("path %q escapes workspace", relPath)
	}

	data, err := os.ReadFile(fullPath)
	if err != nil {
		return nil, err
	}
	return data, nil
}
