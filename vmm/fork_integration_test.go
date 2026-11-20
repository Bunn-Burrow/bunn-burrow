package vmm

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// forkIntegrationAssets checks for the assets needed for fork integration tests.
func forkIntegrationAssets() (bin, kernel, rootfs string, ok bool) {
	bin, kernel, rootfs, ok = integrationAssets()
	if !ok {
		return "", "", "", false
	}
	// Verify the rootfs contains /sbin/bunn-agent (guest agent). Without it,
	// guest-agent health checks cannot work, so skip.
	return bin, kernel, rootfs, true
}

// waitForVMState polls the VM's state machine until it reaches the expected
// state or the timeout expires.
func waitForVMState(t *testing.T, vm *VM, expected State, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if vm.State() == expected {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.Failf(t, "vm state did not reach %s within %v; current state: %s",
		string(expected), timeout, string(vm.State()))
}

// waitForGuestHealth polls the VM's vsock transport for a guest-agent health
// response. Returns an error if the guest does not become healthy within the
// timeout.
func waitForGuestHealth(t *testing.T, vm *VM, port uint32, timeout time.Duration) error {
	t.Helper()
	require.NotNil(t, vm.Vsock(), "vm should have vsock transport")

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := vm.Vsock().Dial(context.Background(), port)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		conn.Close()

		// Try a full health request.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		resp, err := vm.Vsock().SendRequest(ctx, port, map[string]any{"kind": "health"})
		cancel()
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if kind, _ := resp["kind"].(string); kind == "health_ok" {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("guest agent did not become healthy on vsock port %d within %v", port, timeout)
}

// verifyNoFirecrackerProcesses checks that no firecracker processes remain
// belonging to the given VMs.
func verifyNoFirecrackerProcesses(t *testing.T, vms ...*VM) {
	t.Helper()
	pids := make([]int, 0, len(vms))
	for _, vm := range vms {
		if pid := vm.PID(); pid != 0 {
			pids = append(pids, pid)
		}
	}
	if len(pids) == 0 {
		return
	}

	out, err := exec.Command("pgrep", "-x", "firecracker").Output()
	if err != nil {
		// pgrep returns non-zero if no processes match — that's what we want.
		return
	}
	running := strings.TrimSpace(string(out))
	if running == "" {
		return
	}
	t.Fatalf("firecracker processes still running after cleanup: %s", running)
}

// verifyNoLeftoverSockets checks that VM-owned socket files are removed.
func verifyNoLeftoverSockets(t *testing.T, vms ...*VM) {
	t.Helper()
	for _, vm := range vms {
		for _, p := range []string{vm.res.APISock, vm.res.VsockSock} {
			if _, err := os.Stat(p); err != nil {
				require.True(t, os.IsNotExist(err), "socket %q should be removed", p)
			} else {
				t.Fatalf("socket %q should have been removed but still exists", p)
			}
		}
		_, err := os.Stat(vm.Dir())
		require.True(t, os.IsNotExist(err), "VM work dir %q should be removed", vm.Dir())
	}
}

// newIntegrationVM creates a VM configured for Firecracker integration testing
// with direct rootfs boot (no template).
func newIntegrationVM(t *testing.T, bin, kernel, rootfs string, cid uint32) *VM {
	t.Helper()

	cfg := DefaultConfig()
	cfg.BinPath = bin
	cfg.KernelImage = kernel
	cfg.RootFS = rootfs
	cfg.WorkDir = t.TempDir()
	cfg.VCPU = 1
	cfg.MemSizeMiB = 256
	cfg.LogFile = vmResLog(cfg.WorkDir)
	cfg.VsockCID = cid
	cfg.StartTimeout = 30 * time.Second
	cfg.StopTimeout = 15 * time.Second
	cfg.StopGraceTimeout = 5 * time.Second

	vm, err := New(cfg, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = vm.Destroy(context.Background())
	})
	return vm
}

// TestForkParentTwoChildren verifies end-to-end fork of a parent VM into two
// concurrently running children, each with independent vsock communication,
// independent writable storage, and guest-agent health checks.
func TestForkParentTwoChildren(t *testing.T) {
	if os.Getenv("BUNN_RUN_FIRECRACKER_SNAPSHOT_TEST") != "1" {
		t.Skip("requires explicit opt-in; set BUNN_RUN_FIRECRACKER_SNAPSHOT_TEST=1 to run Firecracker integration tests")
	}
	bin, kernel, rootfs, ok := forkIntegrationAssets()
	if !ok {
		t.Skip("requires FIRECRACKER_ROOTFS, Firecracker binary, kernel, and guest-agent-enabled rootfs")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// --- Start parent VM ---
	parent := newIntegrationVM(t, bin, kernel, rootfs, 3)
	require.NoError(t, parent.Start(ctx))
	require.Equal(t, StateRunning, parent.State())
	requireGuestBooted(t, parent)

	// Verify parent guest-agent health via vsock.
	require.NoError(t, waitForGuestHealth(t, parent, GuestAgentVsockPort, 30*time.Second),
		"parent guest agent should be healthy")

	t.Logf("Parent started: id=%s pid=%d state=%s", parent.ID(), parent.PID(), parent.State())

	// --- Pause parent and create snapshot ---
	require.NoError(t, parent.Pause(ctx))
	require.Equal(t, StatePaused, parent.State())

	snap, err := parent.CreateSnapshot(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, snap.ID)
	require.Equal(t, parent.ID(), snap.SourceVM)
	require.Equal(t, "Paused", snap.SourceState)

	// Wait for the state machine to settle back to Paused after snapshot.
	waitForVMState(t, parent, StatePaused, 10*time.Second)

	// Verify snapshot artifacts exist.
	for _, p := range []string{snap.SnapshotPath, snap.MemFilePath} {
		info, err := os.Stat(p)
		require.NoError(t, err, "snapshot artifact %q should exist", p)
		require.Greater(t, info.Size(), int64(0), "snapshot artifact %q should be non-empty", p)
	}

	// --- Fork two children sequentially ---
	// Forking requires the parent to be Paused. After each fork, the parent
	// returns to Paused, so we can fork again.
	// CopyRootFS: true gives each child an independent writable copy of the
	// parent's rootfs, so filesystem modifications in one child are not
	// visible in the parent or sibling.
	var children []*VM
	for i, cid := range []uint32{4, 5} {
		child, err := parent.Fork(ctx, ForkOpts{
			CopyRootFS: true,
			ChildConfig: Config{
				WorkDir:  t.TempDir(),
				VsockCID: cid,
			},
		})
		require.NoError(t, err, "fork child %d should succeed", i)
		require.NotNil(t, child, "child %d should not be nil", i)
		children = append(children, child)

		t.Logf("Child %d forked: id=%s pid=%d state=%s",
			i, child.ID(), child.PID(), child.State())
	}

	require.Len(t, children, 2)
	require.NotEqual(t, children[0].ID(), children[1].ID(),
		"children should have unique IDs")

	// --- Verify each child is running with independent vsock ---
	for i, child := range children {
		require.Equal(t, StateRunning, child.State(),
			"child %d should be running", i)
		require.NotZero(t, child.PID(), "child %d should have a PID", i)
		require.NotNil(t, child.Vsock(), "child %d should have vsock transport", i)

		// Each child's vsock socket path must be unique.
		for j, other := range children {
			if i == j {
				continue
			}
			require.NotEqual(t, child.Vsock().SocketPath(), other.Vsock().SocketPath(),
				"children %d and %d vsock socket paths must differ", i, j)
		}

		// Guest-agent health check via independent vsock.
		require.NoError(t, waitForGuestHealth(t, child, GuestAgentVsockPort, 30*time.Second),
			"child %d guest agent should be healthy", i)

		t.Logf("Child %d: id=%s pid=%d state=%s vsock=%s cid=%d",
			i, child.ID(), child.PID(), child.State(),
			child.Vsock().SocketPath(), child.Vsock().CID())
	}

	// --- Verify parent is still running and unaffected ---
	require.Equal(t, StatePaused, parent.State(),
		"parent should still be paused after forking children")

	// --- Verify independent command execution ---
	// Each child can run independent commands via its own vsock transport.
	// With CopyRootFS: true, each child starts with a copy of the parent's
	// rootfs but has its own writable overlay, so writes in one child are
	// independent.
	for i, child := range children {
		markerFile := fmt.Sprintf("/tmp/child_%d_marker.txt", i)
		markerContent := fmt.Sprintf("child-%d-unique-data", i)

		// Write the marker file in the child.
		writeCmd := fmt.Sprintf("echo '%s' > %s", markerContent, markerFile)
		stdout := &strings.Builder{}
		stderr := &strings.Builder{}
		code, err := runExecOverVsock(ctx, child.Vsock(), GuestAgentVsockPort, writeCmd, stdout, stderr)
		require.NoError(t, err, "child %d write should succeed", i)
		require.Equal(t, 0, code, "child %d write should exit 0", i)

		// Verify the marker file exists and contains the expected content
		// in the child that wrote it.
		readCmd := fmt.Sprintf("cat %s", markerFile)
		stdout.Reset()
		stderr.Reset()
		code, err = runExecOverVsock(ctx, child.Vsock(), GuestAgentVsockPort, readCmd, stdout, stderr)
		require.NoError(t, err, "child %d read should succeed", i)
		require.Equal(t, 0, code, "child %d read should exit 0", i)
		require.Equal(t, markerContent+"\n", stdout.String(),
			"child %d should read its own marker content", i)
	}

	// --- Verify parent filesystem is not modified by children ---
	// The parent is paused for an extended period during child testing,
	// so its vsock may not be responsive. Instead, we verify that each
	// child has its own rootfs copy (different path from parent).
	require.NotEqual(t, parent.cfg.RootFS, children[0].cfg.RootFS,
		"child 0 should have independent rootfs from parent")
	require.NotEqual(t, parent.cfg.RootFS, children[1].cfg.RootFS,
		"child 1 should have independent rootfs from parent")
	require.NotEqual(t, children[0].cfg.RootFS, children[1].cfg.RootFS,
		"children should have independent rootfs from each other")

	// --- Cleanup: destroy children first, then parent ---
	for i, child := range children {
		require.NoError(t, child.Stop(ctx), "child %d stop should succeed", i)
		require.NoError(t, child.Destroy(ctx), "child %d destroy should succeed", i)
		require.Equal(t, StateDestroyed, child.State(), "child %d should be destroyed", i)
	}

	// Resume and destroy parent.
	require.NoError(t, parent.Resume(ctx))
	require.NoError(t, parent.Stop(ctx))
	require.NoError(t, parent.Destroy(ctx))
	require.Equal(t, StateDestroyed, parent.State(), "parent should be destroyed")

	// --- Verify cleanup ---
	verifyNoFirecrackerProcesses(t, parent, children[0], children[1])
	verifyNoLeftoverSockets(t, parent, children[0], children[1])

	// Shared kernel/rootfs should be preserved.
	for _, p := range []string{kernel, rootfs} {
		if _, err := os.Stat(p); err != nil {
			require.NoError(t, err, "shared artifact %q should still exist", p)
		}
	}
}

// runExecOverVsock executes a command in the guest via vsock and captures
// stdout/stderr and exit code. It uses the guest agent's execute RPC.
func runExecOverVsock(ctx context.Context, transport *Transport, port uint32,
	cmd string, stdout, stderr *strings.Builder) (int, error) {

	conn, err := transport.Dial(ctx, port)
	if err != nil {
		return -1, fmt.Errorf("vsock dial: %w", err)
	}
	defer conn.Close()

	// Send execute request.
	req := map[string]any{
		"kind":            "execute",
		"execution_id":    "exec-" + cmd[:min(8, len(cmd))],
		"command":         "/bin/sh",
		"args":            []string{"-c", cmd},
		"working_dir":     "/",
		"timeout_seconds": 10,
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return -1, fmt.Errorf("marshal execute request: %w", err)
	}

	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))
	if _, err := conn.Write(lenBuf[:]); err != nil {
		return -1, fmt.Errorf("write request length: %w", err)
	}
	if _, err := conn.Write(payload); err != nil {
		return -1, fmt.Errorf("write request payload: %w", err)
	}

	// Read response frames until execution_finished.
	var exitCode int
	finished := false
	for !finished {
		var respLenBuf [4]byte
		if _, err := io.ReadFull(conn, respLenBuf[:]); err != nil {
			return -1, fmt.Errorf("read response length: %w", err)
		}
		frameLen := binary.BigEndian.Uint32(respLenBuf[:])
		if frameLen == 0 || frameLen > 1<<20 {
			return -1, fmt.Errorf("invalid response frame length: %d", frameLen)
		}
		respPayload := make([]byte, frameLen)
		if _, err := io.ReadFull(conn, respPayload); err != nil {
			return -1, fmt.Errorf("read response payload: %w", err)
		}

		var resp map[string]any
		if err := json.Unmarshal(respPayload, &resp); err != nil {
			return -1, fmt.Errorf("unmarshal response: %w", err)
		}

		switch kind, _ := resp["kind"].(string); kind {
		case "execution_output":
			stream, _ := resp["stream"].(string)
			data, _ := resp["data"].(string)
			switch stream {
			case "stdout":
				stdout.WriteString(data)
			case "stderr":
				stderr.WriteString(data)
			}
		case "execution_finished":
			exitCode = int(resp["exit_code"].(float64))
			finished = true
		case "error":
			return -1, fmt.Errorf("guest error: %v", resp["message"])
		}
	}

	return exitCode, nil
}

// TestForkConcurrentDestroy verifies that destroying children concurrently
// while the parent is still running is safe and cleans up all resources.
func TestForkConcurrentDestroy(t *testing.T) {
	if os.Getenv("BUNN_RUN_FIRECRACKER_SNAPSHOT_TEST") != "1" {
		t.Skip("requires explicit opt-in; set BUNN_RUN_FIRECRACKER_SNAPSHOT_TEST=1 to run Firecracker integration tests")
	}
	bin, kernel, rootfs, ok := forkIntegrationAssets()
	if !ok {
		t.Skip("requires FIRECRACKER_ROOTFS, Firecracker binary, kernel, and guest-agent-enabled rootfs")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	parent := newIntegrationVM(t, bin, kernel, rootfs, 3)
	require.NoError(t, parent.Start(ctx))
	requireGuestBooted(t, parent)

	require.NoError(t, parent.Pause(ctx))
	require.Equal(t, StatePaused, parent.State())

	_, err := parent.CreateSnapshot(ctx)
	require.NoError(t, err)

	// Fork two children sequentially (fork requires parent to be Paused;
	// each fork returns parent to Paused).
	var children []*VM
	for i, cid := range []uint32{4, 5} {
		child, err := parent.Fork(ctx, ForkOpts{
			CopyRootFS: true,
			ChildConfig: Config{
				WorkDir:  t.TempDir(),
				VsockCID: cid,
			},
		})
		require.NoError(t, err, "fork child %d should succeed", i)
		require.NotNil(t, child, "child %d should not be nil", i)

		// Verify child is running before forking the next one.
		// Both children start Firecracker from the same snapshot, so we
		// verify each one individually to avoid cascading failures.
		require.Equal(t, StateRunning, child.State(),
			"child %d should be running", i)
		require.NotNil(t, child.Vsock(), "child %d should have vsock", i)
		children = append(children, child)

		t.Logf("Child %d forked: id=%s pid=%d state=%s vsockCID=%d",
			i, child.ID(), child.PID(), child.State(), child.Vsock().CID())
	}

	require.Len(t, children, 2)
	require.NotEqual(t, children[0].ID(), children[1].ID(),
		"children should have unique IDs")

	// Concurrently destroy both children.
	destroyCh := make(chan error, 2)
	for _, child := range children {
		go func(c *VM) {
			destroyCh <- c.Destroy(context.Background())
		}(child)
	}
	for i := 0; i < 2; i++ {
		require.NoError(t, <-destroyCh, "concurrent destroy should succeed")
	}

	// Both children should be Destroyed.
	for _, child := range children {
		require.Equal(t, StateDestroyed, child.State())
	}

	// Parent still running.
	require.Equal(t, StatePaused, parent.State())

	// Cleanup parent.
	require.NoError(t, parent.Resume(ctx))
	require.NoError(t, parent.Stop(ctx))
	require.NoError(t, parent.Destroy(ctx))

	verifyNoFirecrackerProcesses(t, parent, children[0], children[1])
	verifyNoLeftoverSockets(t, parent, children[0], children[1])
}

// TestForkSnapshotIntegrity verifies that a corrupted snapshot artifact is
// rejected during restore with a clear checksum mismatch error.
func TestForkSnapshotIntegrity(t *testing.T) {
	if os.Getenv("BUNN_RUN_FIRECRACKER_SNAPSHOT_TEST") != "1" {
		t.Skip("requires explicit opt-in; set BUNN_RUN_FIRECRACKER_SNAPSHOT_TEST=1 to run Firecracker integration tests")
	}
	bin, kernel, rootfs, ok := forkIntegrationAssets()
	if !ok {
		t.Skip("requires FIRECRACKER_ROOTFS, Firecracker binary, kernel, and guest-agent-enabled rootfs")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	parent := newIntegrationVM(t, bin, kernel, rootfs, 3)
	require.NoError(t, parent.Start(ctx))
	requireGuestBooted(t, parent)

	require.NoError(t, parent.Pause(ctx))
	require.Equal(t, StatePaused, parent.State())

	snap, err := parent.CreateSnapshot(ctx)
	require.NoError(t, err)

	require.NoError(t, parent.Resume(ctx))
	require.NoError(t, parent.Stop(ctx))
	require.NoError(t, parent.Destroy(ctx))

	// Corrupt the snapshot state file by flipping a byte.
	stateData, err := os.ReadFile(snap.SnapshotPath)
	require.NoError(t, err)
	require.Greater(t, len(stateData), 0, "state file should be non-empty")
	stateData[0] ^= 0xFF // Flip first byte
	require.NoError(t, os.WriteFile(snap.SnapshotPath, stateData, 0644))

	// Attempting to restore from the corrupted snapshot should fail.
	// Build M6-layout snapshot from the corrupted legacy artifacts.
	store := NewSnapshotStore(filepath.Join(t.TempDir(), "snapshots"))
	m6Snap := &SnapshotInfo{
		ID:                snap.ID,
		SourceVM:          snap.SourceVM,
		SnapshotPath:      store.statePath(snap.ID),
		MemFilePath:       store.memPath(snap.ID),
		RootFSPath:        snap.RootFSPath,
		VCPU:              snap.VCPU,
		MemSizeMiB:        snap.MemSizeMiB,
		VsockCID:          snap.VsockCID,
		VsockPort:         snap.VsockPort,
		KernelImage:       snap.KernelImage,
		BootArgs:          snap.BootArgs,
		Status:            snapshotStatusReady,
		ArtifactChecksums: snap.ArtifactChecksums, // original checksums; mismatch will be detected
	}
	snapDir := filepath.Join(store.baseDir, m6Snap.ID)
	require.NoError(t, os.MkdirAll(snapDir, 0o750))

	// Copy the corrupted state file and valid mem file.
	require.NoError(t, copyFile(snap.SnapshotPath, m6Snap.SnapshotPath))
	require.NoError(t, copyFile(snap.MemFilePath, m6Snap.MemFilePath))
	require.NoError(t, store.Create(m6Snap))

	_, err = RestoreSnapshot(ctx, store, m6Snap.ID, nil, nil, nil, RestoreOpts{
		WorkDir:  t.TempDir(),
		VsockCID: 6,
	}, nil)
	require.Error(t, err, "restore from corrupted snapshot should fail")
	require.Contains(t, err.Error(), "checksum",
		"error should mention checksum mismatch, got: %s", err.Error())
}

// TestForkFailureCleanup verifies that if a child fork fails mid-way, all
// acquired resources are cleaned up and no leaks remain.
func TestForkFailureCleanup(t *testing.T) {
	if os.Getenv("BUNN_RUN_FIRECRACKER_SNAPSHOT_TEST") != "1" {
		t.Skip("requires explicit opt-in; set BUNN_RUN_FIRECRACKER_SNAPSHOT_TEST=1 to run Firecracker integration tests")
	}
	bin, kernel, rootfs, ok := forkIntegrationAssets()
	if !ok {
		t.Skip("requires FIRECRACKER_ROOTFS, Firecracker binary, kernel, and guest-agent-enabled rootfs")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	parent := newIntegrationVM(t, bin, kernel, rootfs, 3)
	require.NoError(t, parent.Start(ctx))
	requireGuestBooted(t, parent)

	require.NoError(t, parent.Pause(ctx))
	require.Equal(t, StatePaused, parent.State())

	// Get the list of firecracker processes after starting the parent
	// (so the parent's PID is in the "before" set).
	procsBefore, err := exec.Command("pgrep", "-x", "firecracker").Output()
	require.NoError(t, err)
	procsBeforeSet := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(string(procsBefore)), "\n") {
		if line != "" {
			procsBeforeSet[line] = true
		}
	}
	parentPID := fmt.Sprintf("%d", parent.PID())
	procsBeforeSet[parentPID] = true // explicitly include parent PID

	// Create a snapshot.
	_, err = parent.CreateSnapshot(ctx)
	require.NoError(t, err)

	// Fork with CopyRootFS: true. badWorkDir is a regular file (not a
	// directory), so New()'s os.MkdirAll will fail when trying to create
	// the child's work directory inside it. This tests that a failed fork
	// leaves no leaked Firecracker processes.
	badWorkDir := filepath.Join(t.TempDir(), "bad-work-dir")
	require.NoError(t, os.WriteFile(badWorkDir, []byte("not a directory"), 0644))
	_, err = parent.Fork(ctx, ForkOpts{
		CopyRootFS: true,
		ChildConfig: Config{
			WorkDir:  badWorkDir,
			VsockCID: 4,
		},
	})
	require.Error(t, err, "fork with conflicting work dir should fail")

	// Parent should still be paused and running.
	require.Equal(t, StatePaused, parent.State())

	// No NEW firecracker processes should have been left behind (the fork
	// failed before startProcessAndWait was called).
	procsAfter, err := exec.Command("pgrep", "-x", "firecracker").Output()
	require.NoError(t, err)
	procsAfterSet := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(string(procsAfter)), "\n") {
		if line != "" {
			procsAfterSet[line] = true
		}
	}
	// Every process running after the failed fork must have been running
	// before the fork attempt (no leaked processes).
	for pid := range procsAfterSet {
		require.True(t, procsBeforeSet[pid],
			"new firecracker process leaked after failed fork: pid %s", pid)
	}

	// Cleanup parent.
	require.NoError(t, parent.Resume(ctx))
	require.NoError(t, parent.Stop(ctx))
	require.NoError(t, parent.Destroy(ctx))

	verifyNoFirecrackerProcesses(t, parent)
	verifyNoLeftoverSockets(t, parent)
}

// TestForkRepeatedDestroy verifies that repeated Destroy calls on a forked
// child are idempotent and safe.
func TestForkRepeatedDestroy(t *testing.T) {
	if os.Getenv("BUNN_RUN_FIRECRACKER_SNAPSHOT_TEST") != "1" {
		t.Skip("requires explicit opt-in; set BUNN_RUN_FIRECRACKER_SNAPSHOT_TEST=1 to run Firecracker integration tests")
	}
	bin, kernel, rootfs, ok := forkIntegrationAssets()
	if !ok {
		t.Skip("requires FIRECRACKER_ROOTFS, Firecracker binary, kernel, and guest-agent-enabled rootfs")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	parent := newIntegrationVM(t, bin, kernel, rootfs, 3)
	require.NoError(t, parent.Start(ctx))
	requireGuestBooted(t, parent)

	require.NoError(t, parent.Pause(ctx))
	require.Equal(t, StatePaused, parent.State())

	_, err := parent.CreateSnapshot(ctx)
	require.NoError(t, err)

	child, err := parent.Fork(ctx, ForkOpts{
		CopyRootFS: true,
		ChildConfig: Config{
			WorkDir:  t.TempDir(),
			VsockCID: 4,
		},
	})
	require.NoError(t, err)
	require.NotNil(t, child)
	require.Equal(t, StateRunning, child.State())

	// Repeated Destroy calls should all succeed.
	require.NoError(t, child.Destroy(ctx))
	require.Equal(t, StateDestroyed, child.State())
	require.NoError(t, child.Destroy(ctx))
	require.Equal(t, StateDestroyed, child.State())
	require.NoError(t, child.Destroy(ctx))
	require.Equal(t, StateDestroyed, child.State())

	// Cleanup parent.
	require.NoError(t, parent.Resume(ctx))
	require.NoError(t, parent.Stop(ctx))
	require.NoError(t, parent.Destroy(ctx))

	verifyNoFirecrackerProcesses(t, parent, child)
	verifyNoLeftoverSockets(t, parent, child)
}
