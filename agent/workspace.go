package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/omkar/sandbox/vmm"
)

const (
	// MaxWorkspaceFiles is the maximum number of files allowed in an agent workspace payload.
	MaxWorkspaceFiles = 1000

	// MaxWorkspaceTotalSize is the maximum combined size of workspace files (100 MiB).
	MaxWorkspaceTotalSize = 100 * 1024 * 1024

	// MaxWorkspaceFileSize is the maximum individual file size (10 MiB).
	MaxWorkspaceFileSize = 10 * 1024 * 1024
)

// WorkspaceLockManager provides mutual exclusion locks per workspace ID to ensure
// safe concurrent workspace semantics. Concurrent agent executions targeting the same
// workspace ID are serialized to avoid clobbering or corrupting files.
type WorkspaceLockManager struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// NewWorkspaceLockManager creates a new workspace lock manager.
func NewWorkspaceLockManager() *WorkspaceLockManager {
	return &WorkspaceLockManager{
		locks: make(map[string]*sync.Mutex),
	}
}

// getMutex returns the mutex for a workspace ID, creating one if needed.
func (m *WorkspaceLockManager) getMutex(workspaceID string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()

	l, ok := m.locks[workspaceID]
	if !ok {
		l = &sync.Mutex{}
		m.locks[workspaceID] = l
	}
	return l
}

// Acquire locks the workspace for exclusive access until the returned unlock function is called.
// If ctx is cancelled before acquiring the lock, an error is returned.
func (m *WorkspaceLockManager) Acquire(ctx context.Context, workspaceID string) (func(), error) {
	if workspaceID == "" {
		// Unnamed workspace needs no lock
		return func() {}, nil
	}

	l := m.getMutex(workspaceID)

	acquired := make(chan struct{})
	go func() {
		l.Lock()
		close(acquired)
	}()

	select {
	case <-ctx.Done():
		// In case lock is acquired right after timeout, release it
		go func() {
			<-acquired
			l.Unlock()
		}()
		return nil, fmt.Errorf("timed out acquiring workspace lock for %q: %w", workspaceID, ctx.Err())
	case <-acquired:
		var once sync.Once
		unlock := func() {
			once.Do(func() {
				l.Unlock()
			})
		}
		return unlock, nil
	}
}

// ValidateWorkspacePath validates a file path inside a workspace.
// It rejects absolute paths, paths containing '..', and empty paths.
func ValidateWorkspacePath(path string) error {
	if path == "" {
		return errors.New("empty file path")
	}
	if filepath.IsAbs(path) || strings.HasPrefix(path, "/") {
		return fmt.Errorf("absolute paths not permitted in workspace: %q", path)
	}
	clean := filepath.Clean(path)
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return fmt.Errorf("directory traversal ('..') not permitted in workspace path: %q", path)
	}
	return nil
}

// ValidateWorkspaceFiles validates the files map of a workspace payload against safety and size limits.
func ValidateWorkspaceFiles(files map[string][]byte) error {
	if len(files) > MaxWorkspaceFiles {
		return fmt.Errorf("workspace file count %d exceeds maximum limit of %d", len(files), MaxWorkspaceFiles)
	}

	var totalSize int64
	for path, content := range files {
		if err := ValidateWorkspacePath(path); err != nil {
			return err
		}
		size := int64(len(content))
		if size > MaxWorkspaceFileSize {
			return fmt.Errorf("file %q size (%d bytes) exceeds maximum of %d bytes", path, size, MaxWorkspaceFileSize)
		}
		totalSize += size
		if totalSize > MaxWorkspaceTotalSize {
			return fmt.Errorf("total workspace size exceeds maximum of %d bytes", MaxWorkspaceTotalSize)
		}
	}

	return nil
}

// BuildProject converts a WorkspaceConfig into a vmm.Project ready for guest upload.
// Note on Persistent Volumes: W4 provides host-side NBD block volume management,
// but guest filesystem formatting and mounting are not yet implemented in the guest agent.
// Therefore, the project filesystem upload mechanism (W2) is used as the active workspace,
// while preserving any VolumeID reference for tracking.
func BuildProject(ws *WorkspaceConfig, defaultID string) (*vmm.Project, error) {
	if ws == nil {
		return nil, nil
	}

	if err := ValidateWorkspaceFiles(ws.Files); err != nil {
		return nil, fmt.Errorf("invalid workspace files: %w", err)
	}

	id := ws.ID
	if id == "" {
		id = defaultID
	}

	guestPath := ws.GuestPath
	if guestPath == "" {
		guestPath = fmt.Sprintf("/tmp/bunn-workspace/%s", id)
	}

	return &vmm.Project{
		ID:           id,
		Files:        ws.Files,
		WorkspaceDir: guestPath,
	}, nil
}
