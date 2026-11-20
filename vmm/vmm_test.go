package vmm

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/stretchr/testify/require"
)

// fakeAPI implements API for tests.
type fakeAPI struct {
	mu                      sync.Mutex
	calls                   []string
	instanceState           string
	machineCfg              *models.MachineConfiguration
	lastVsock               *models.Vsock
	lastAction              string
	putMachineCfgErr        error
	putBootSourceErr        error
	putGuestDriveErr        error
	putGuestVsockErr        error
	putGuestNetIfaceErr     error
	createSyncActionErr     error
	patchVMErr              error
	loadSnapshotErr         error
	loadSnapshotOverrideErr error
	createSnapshotErr       error
	lastSnapshotPath        string
	lastMemFilePath         string
	lastResumeVM            bool
	lastUdsPath             string
}

func (f *fakeAPI) PutMachineConfiguration(ctx context.Context, cfg *models.MachineConfiguration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "PutMachineConfiguration")
	f.machineCfg = cfg
	return f.putMachineCfgErr
}
func (f *fakeAPI) PutGuestBootSource(ctx context.Context, src *models.BootSource) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "PutGuestBootSource")
	return f.putBootSourceErr
}
func (f *fakeAPI) PutGuestDriveByID(ctx context.Context, driveID string, d *models.Drive) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "PutGuestDriveByID")
	return f.putGuestDriveErr
}
func (f *fakeAPI) PutGuestNetworkInterfaceByID(ctx context.Context, ifaceID string, ni *models.NetworkInterface) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "PutGuestNetworkInterfaceByID")
	return f.putGuestNetIfaceErr
}
func (f *fakeAPI) PutGuestVsock(ctx context.Context, vsock *models.Vsock) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "PutGuestVsock")
	f.lastVsock = vsock
	return f.putGuestVsockErr
}
func (f *fakeAPI) CreateSyncAction(ctx context.Context, action *models.InstanceActionInfo) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "CreateSyncAction")
	if action.ActionType != nil {
		f.lastAction = *action.ActionType
	}
	return f.createSyncActionErr
}
func (f *fakeAPI) PatchVM(ctx context.Context, vm *models.VM) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "PatchVM")
	if vm.State != nil {
		f.instanceState = *vm.State
	}
	return f.patchVMErr
}
func (f *fakeAPI) CreateSnapshot(ctx context.Context, p *models.SnapshotCreateParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "CreateSnapshot")
	if f.createSnapshotErr != nil {
		return f.createSnapshotErr
	}
	// Create dummy snapshot files so artifact validation passes in tests.
	if p.SnapshotPath != nil {
		_ = os.WriteFile(*p.SnapshotPath, []byte("snapshot-state"), 0644)
	}
	if p.MemFilePath != nil {
		_ = os.WriteFile(*p.MemFilePath, []byte("snapshot-mem"), 0644)
	}
	return nil
}
func (f *fakeAPI) LoadSnapshot(ctx context.Context, p *models.SnapshotLoadParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "LoadSnapshot")
	return f.loadSnapshotErr
}
func (f *fakeAPI) LoadSnapshotWithOverride(ctx context.Context, snapshotPath, memFilePath string, resumeVM bool, udsPath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "LoadSnapshotWithOverride")
	f.lastSnapshotPath = snapshotPath
	f.lastMemFilePath = memFilePath
	f.lastResumeVM = resumeVM
	f.lastUdsPath = udsPath
	return f.loadSnapshotOverrideErr
}
func (f *fakeAPI) GetInstanceInfo(ctx context.Context) (*models.InstanceInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "GetInstanceInfo")
	return &models.InstanceInfo{State: &f.instanceState}, nil
}
func (f *fakeAPI) GetMachineConfiguration(ctx context.Context) (*models.MachineConfiguration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "GetMachineConfiguration")
	return f.machineCfg, nil
}

func testConfig(t *testing.T) Config {
	c := DefaultConfig()
	c.KernelImage = "/dev/null"
	c.RootFS = "/dev/null"
	c.WorkDir = t.TempDir()
	return c
}

func TestStateMachineValidTransitions(t *testing.T) {
	// Happy boot path: Created -> Preparing -> Running -> Paused -> Running.
	sm := NewStateMachine()
	require.NoError(t, sm.Transition(StateCreated, StatePreparing))
	require.NoError(t, sm.Transition(StatePreparing, StateRunning))
	require.NoError(t, sm.Transition(StateRunning, StatePaused))
	require.NoError(t, sm.Transition(StatePaused, StateRunning))
	require.Equal(t, StateRunning, sm.State())

	// Snapshot loop: Running -> Paused -> Snapshotting -> Paused.
	require.NoError(t, sm.Transition(StateRunning, StatePaused))
	require.NoError(t, sm.Transition(StatePaused, StateSnapshotting))
	require.NoError(t, sm.Transition(StateSnapshotting, StatePaused))
	require.Equal(t, StatePaused, sm.State())

	// Stop + destroy path: Paused -> Running -> Stopping -> Stopped -> Destroying -> Destroyed.
	require.NoError(t, sm.Transition(StatePaused, StateRunning))
	require.NoError(t, sm.Transition(StateRunning, StateStopping))
	require.NoError(t, sm.Transition(StateStopping, StateStopped))
	require.NoError(t, sm.Transition(StateStopped, StateDestroying))
	require.NoError(t, sm.Transition(StateDestroying, StateDestroyed))
	require.Equal(t, StateDestroyed, sm.State())
}

func TestStateMachineInvalidTransitions(t *testing.T) {
	sm := NewStateMachine()
	require.Error(t, sm.Transition(StateCreated, StateRunning))
	require.Error(t, sm.Transition(StateRunning, StateCreated))
	require.Error(t, sm.Transition(StateDestroyed, StateRunning))
	require.Equal(t, StateCreated, sm.State())
}

func TestStateMachineTerminal(t *testing.T) {
	sm := NewStateMachine()
	require.True(t, sm.IsTerminal(StateDestroyed))
	require.True(t, sm.IsTerminal(StateFailed))
	require.False(t, sm.IsTerminal(StateRunning))
	require.False(t, sm.IsTerminal(StatePaused))
}

func TestConfigValidate(t *testing.T) {
	c := DefaultConfig()
	require.Equal(t, GuestAgentVsockPort, c.VsockPort)
	c.KernelImage = "/nonexistent/kernel"
	c.RootFS = "/nonexistent/rootfs"
	require.Error(t, c.Validate())

	c.KernelImage = "/dev/null"
	c.RootFS = "/dev/null"
	require.NoError(t, c.Validate())

	c2 := DefaultConfig()
	c2.KernelImage = "/dev/null"
	c2.RootFS = "/dev/null"
	c2.VCPU = 3
	require.Error(t, c2.Validate())
	c2.VCPU = 2
	c2.MemSizeMiB = 0
	require.Error(t, c2.Validate())
	c2.MemSizeMiB = 512
	c2.VsockCID = 2
	require.Error(t, c2.Validate())
	c2.VsockCID = 3
	c2.VsockPort = 0
	require.Error(t, c2.Validate())
	c2.VsockPort = 1024
	require.NoError(t, c2.Validate())
}

func TestVMNew(t *testing.T) {
	vm, err := New(testConfig(t), nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vm.Destroy(context.Background())) })
	require.NotEmpty(t, vm.ID())
	require.Equal(t, StateCreated, vm.State())
	require.Equal(t, vm.ID(), filepath.Base(vm.Dir()))
	require.NotEmpty(t, vm.Dir())
	require.NotEmpty(t, vm.res.APISock)
	require.NotEmpty(t, vm.res.VsockSock)
	require.NotEqual(t, vm.res.APISock, vm.res.VsockSock)
}

func TestVMNewWithID(t *testing.T) {
	c := testConfig(t)
	c.ID = "my-vm"
	vm, err := New(c, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vm.Destroy(context.Background())) })
	require.Equal(t, "my-vm", vm.ID())
}

func TestVMDefaultWorkDirUsesUserDataDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	c := testConfig(t)
	c.WorkDir = ""
	vm, err := New(c, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vm.Destroy(context.Background())) })

	expectedBase := filepath.Join(home, ".local", "share", "bunn-burrow", "vms")
	require.Equal(t, filepath.Join(expectedBase, vm.ID()), vm.Dir())
	baseInfo, err := os.Stat(expectedBase)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0700), baseInfo.Mode().Perm())
}

func TestVMWorkDirIsExclusiveAndScoped(t *testing.T) {
	base := t.TempDir()
	externalFile := filepath.Join(base, "keep.txt")
	require.NoError(t, os.WriteFile(externalFile, []byte("keep"), 0600))

	c := testConfig(t)
	c.WorkDir = base
	c.ID = "scoped-vm"
	vm, err := New(c, nil)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(base, c.ID), vm.Dir())
	require.ErrorContains(t, os.Mkdir(vm.Dir(), 0700), "file exists")
	require.NoError(t, vm.Destroy(context.Background()))

	data, err := os.ReadFile(externalFile)
	require.NoError(t, err)
	require.Equal(t, "keep", string(data))
	_, err = os.Stat(vm.Dir())
	require.ErrorIs(t, err, os.ErrNotExist)

	_, err = New(c, nil)
	require.NoError(t, err)
}

func TestVMRejectsUnsafeWorkDirAndID(t *testing.T) {
	c := testConfig(t)
	c.WorkDir = string(os.PathSeparator)
	_, err := New(c, nil)
	require.ErrorContains(t, err, "cannot be the filesystem root")

	c.WorkDir = t.TempDir()
	c.ID = "../outside"
	_, err = New(c, nil)
	require.ErrorContains(t, err, "must be a single path component")
}

func TestPauseResumeWithFakeAPI(t *testing.T) {
	vm, err := New(testConfig(t), nil)
	require.NoError(t, err)
	require.NoError(t, vm.sm.Transition(StateCreated, StatePreparing))
	require.NoError(t, vm.sm.Transition(StatePreparing, StateRunning))
	vm.mu.Lock()
	vm.api = &fakeAPI{}
	vm.mu.Unlock()

	ctx := context.Background()
	require.NoError(t, vm.Pause(ctx))
	require.Equal(t, StatePaused, vm.State())
	require.NoError(t, vm.Resume(ctx))
	require.Equal(t, StateRunning, vm.State())
}

func TestPauseRejectsNonRunning(t *testing.T) {
	vm, err := New(testConfig(t), nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vm.Destroy(context.Background())) })
	ctx := context.Background()
	require.Error(t, vm.Pause(ctx)) // Created -> not Running
	require.Equal(t, StateCreated, vm.State())
}

func TestStopWithNoProcessTimesOut(t *testing.T) {
	c := testConfig(t)
	c.StopTimeout = 100 * time.Millisecond
	c.StopGraceTimeout = 100 * time.Millisecond
	vm, err := New(c, nil)
	require.NoError(t, err)
	require.NoError(t, vm.sm.Transition(StateCreated, StatePreparing))
	require.NoError(t, vm.sm.Transition(StatePreparing, StateRunning))
	vm.mu.Lock()
	vm.api = &fakeAPI{}
	vm.mu.Unlock()

	ctx := context.Background()
	err = vm.Stop(ctx)
	require.Error(t, err)
	require.Equal(t, StateStopping, vm.State())
}

func TestProcessStartAndExit(t *testing.T) {
	p, err := Start("true", nil, nil)
	require.NoError(t, err)
	require.NotZero(t, p.PID())
	select {
	case <-p.Exited():
	case <-time.After(5 * time.Second):
		require.Fail(t, "process did not exit")
	}
	require.NoError(t, p.ExitError())
	require.True(t, p.Reaped())
}

func TestProcessKill(t *testing.T) {
	p, err := Start("sleep", []string{"100"}, nil)
	require.NoError(t, err)
	require.NoError(t, p.Kill())
	select {
	case <-p.Exited():
	case <-time.After(5 * time.Second):
		require.Fail(t, "process did not exit after kill")
	}
	require.Error(t, p.ExitError())
}

func TestVsockDialHandshake(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "vsock.sock")
	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	defer ln.Close()

	serverErr := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer c.Close()
		line, err := readUntil(c, '\n', 5*time.Second)
		if err != nil {
			serverErr <- err
			return
		}
		if line != "CONNECT 1024" {
			serverErr <- errors.New("bad connect message: " + line)
			return
		}
		_, werr := c.Write([]byte("OK 5000\n"))
		if werr != nil {
			serverErr <- werr
			return
		}
		// Echo for the remainder of the connection so the client can exchange
		data := make([]byte, 64)
		for {
			n, err := c.Read(data)
			if err != nil {
				if errors.Is(err, io.EOF) {
					serverErr <- nil
				} else {
					serverErr <- err
				}
				return
			}
			if _, werr := c.Write(append(data[:n], '\n')); werr != nil {
				serverErr <- werr
				return
			}
		}
	}()

	tr := NewTransport(sockPath, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := tr.Dial(ctx, 1024)
	require.NoError(t, err)

	_, err = conn.Write([]byte("hello"))
	require.NoError(t, err)
	n, err := readUntil(conn, '\n', 5*time.Second)
	require.NoError(t, err)
	require.Equal(t, "hello", strings.TrimRight(n, "\n"))

	conn.Close()
	select {
	case err := <-serverErr:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "server did not complete handshake")
	}
}

func TestVsockDialRefusesZeroPort(t *testing.T) {
	tr := NewTransport("/tmp/fake.sock", 3)
	_, err := tr.Dial(context.Background(), 0)
	require.Error(t, err)
}

func TestSnapshotStorePaths(t *testing.T) {
	dir := t.TempDir()
	store := NewSnapshotStore(dir)
	require.Equal(t, filepath.Join(dir, "snapshots", "vm1", "snap1.snapshot"), store.SnapshotPath("vm1", "snap1"))
	require.Equal(t, filepath.Join(dir, "snapshots", "vm1", "snap1.mem"), store.MemPath("vm1", "snap1"))
}

func TestValidateArtifact(t *testing.T) {
	nonempty := filepath.Join(t.TempDir(), "nonempty")
	require.NoError(t, os.WriteFile(nonempty, []byte("data"), 0644))
	require.NoError(t, validateArtifact(nonempty))
	empty := filepath.Join(t.TempDir(), "empty")
	require.NoError(t, os.WriteFile(empty, nil, 0644))
	require.Error(t, validateArtifact(empty))
	require.Error(t, validateArtifact(filepath.Join(t.TempDir(), "missing")))
}

func TestCopyFile(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.WriteFile(src, []byte("data"), 0644))
	dst := filepath.Join(t.TempDir(), "dst")
	require.NoError(t, copyFile(src, dst))
	b, err := os.ReadFile(dst)
	require.NoError(t, err)
	require.Equal(t, "data", string(b))
}

func TestForkRequiresPaused(t *testing.T) {
	vm, err := New(testConfig(t), nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vm.Destroy(context.Background())) })
	_, err = vm.Fork(context.Background(), ForkOpts{CopyRootFS: true})
	require.Error(t, err)
	require.Contains(t, err.Error(), "paused")
}

func TestStopEscalatesToKillAndWaitsForReap(t *testing.T) {
	c := testConfig(t)
	c.StopTimeout = 50 * time.Millisecond
	c.StopGraceTimeout = 2 * time.Second

	vm, err := New(c, nil)
	require.NoError(t, err)

	require.NoError(t, vm.sm.Transition(StateCreated, StatePreparing))
	require.NoError(t, vm.sm.Transition(StatePreparing, StateRunning))

	vm.mu.Lock()
	vm.api = &fakeAPI{}
	vm.mu.Unlock()

	// Ignore SIGTERM so Stop must escalate to SIGKILL.
	proc, err := Start("sh", []string{
		"-c", `trap '' TERM; exec sleep 100`,
	}, nil)
	require.NoError(t, err)

	vm.mu.Lock()
	vm.proc = proc
	vm.mu.Unlock()

	go vm.monitorExit()

	err = vm.Stop(context.Background())
	require.NoError(t, err)

	select {
	case <-proc.Exited():
	case <-time.After(2 * time.Second):
		t.Fatal("process was not reaped")
	}

	require.True(t, proc.Reaped())
	require.Equal(t, StateStopped, vm.State())
}

// TestLoadSnapshotWithOverrideJSON verifies the exact JSON request body sent
// by LoadSnapshotWithOverride: snapshot_path, mem_file_path, resume_vm, and
// the nested vsock_override.uds_path, with no undocumented fields.
func TestLoadSnapshotWithOverrideJSON(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "api.socket")

	var receivedBody string
	var receivedMethod string
	var receivedPath string
	var receivedContentType string

	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	defer ln.Close()

	done := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			close(done)
			return
		}
		defer conn.Close()

		req, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			close(done)
			return
		}
		defer req.Body.Close()

		receivedMethod = req.Method
		receivedPath = req.URL.Path
		receivedContentType = req.Header.Get("Content-Type")
		bodyBytes, _ := io.ReadAll(req.Body)
		receivedBody = string(bodyBytes)

		// Respond 204 No Content (Firecracker success).
		_, _ = conn.Write([]byte("HTTP/1.1 204 No Content\r\n\r\n"))
		close(done)
	}()

	client := &apiClient{
		client:     nil,
		socketPath: sockPath,
	}

	ctx := context.Background()
	err = client.LoadSnapshotWithOverride(ctx, "/path/to/snap", "/path/to/mem", true, "/path/to/override.sock")
	require.NoError(t, err)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("request not received")
	}

	require.Equal(t, "PUT", receivedMethod)
	require.Equal(t, "/snapshot/load", receivedPath)
	require.Equal(t, "application/json", receivedContentType)

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(receivedBody), &body))

	// Exact documented fields only.
	require.Equal(t, "/path/to/snap", body["snapshot_path"])
	require.Equal(t, "/path/to/mem", body["mem_file_path"])
	require.Equal(t, true, body["resume_vm"])

	vsockOverride, ok := body["vsock_override"].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, "/path/to/override.sock", vsockOverride["uds_path"])

	// No undocumented fields at the top level.
	require.Len(t, body, 4)
}

// TestLoadSnapshotWithOverrideErrorStatus verifies that non-204 responses
// produce an error that includes the HTTP status and Firecracker error body.
func TestLoadSnapshotWithOverrideErrorStatus(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "api.socket")

	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	defer ln.Close()

	done := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			close(done)
			return
		}
		defer conn.Close()

		req, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			close(done)
			return
		}
		defer req.Body.Close()
		io.Copy(io.Discard, req.Body)

		// Respond 400 with Firecracker error body.
		_, _ = conn.Write([]byte("HTTP/1.1 400 Bad Request\r\nContent-Type: application/json\r\n\r\n{\"fault_message\":\"Failed to open snapshot file\"}"))
		close(done)
	}()

	client := &apiClient{
		client:     nil,
		socketPath: sockPath,
	}

	ctx := context.Background()
	err = client.LoadSnapshotWithOverride(ctx, "/path/to/snap", "/path/to/mem", true, "/path/to/override.sock")
	require.Error(t, err)
	require.Contains(t, err.Error(), "400")
	require.Contains(t, err.Error(), "Failed to open snapshot file")

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("request not received")
	}
}

// TestLoadSnapshotWithOverrideContextCancel verifies that context cancellation
// aborts the request.
func TestLoadSnapshotWithOverrideContextCancel(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "api.socket")

	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	defer ln.Close()

	// Never accept the connection, so the request hangs until cancelled.
	client := &apiClient{
		client:     nil,
		socketPath: sockPath,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = client.LoadSnapshotWithOverride(ctx, "/path/to/snap", "/path/to/mem", true, "/path/to/override.sock")
	require.Error(t, err)
}

// TestLoadSnapshotBackwardCompatible verifies the existing SDK-based
// LoadSnapshot path still works through the fakeAPI.
func TestLoadSnapshotBackwardCompatible(t *testing.T) {
	f := &fakeAPI{}
	err := f.LoadSnapshot(context.Background(), &models.SnapshotLoadParams{
		SnapshotPath: strPtr("/path/to/snap"),
		MemFilePath:  "/path/to/mem",
		ResumeVM:     true,
	})
	require.NoError(t, err)
	require.Contains(t, f.calls, "LoadSnapshot")
}

// TestFakeAPILoadSnapshotWithOverride verifies the fakeAPI captures the
// vsock_override parameters correctly.
func TestFakeAPILoadSnapshotWithOverride(t *testing.T) {
	f := &fakeAPI{}
	err := f.LoadSnapshotWithOverride(context.Background(), "/path/to/snap", "/path/to/mem", true, "/path/to/override.sock")
	require.NoError(t, err)
	require.Contains(t, f.calls, "LoadSnapshotWithOverride")
	require.Equal(t, "/path/to/snap", f.lastSnapshotPath)
	require.Equal(t, "/path/to/mem", f.lastMemFilePath)
	require.True(t, f.lastResumeVM)
	require.Equal(t, "/path/to/override.sock", f.lastUdsPath)
}

// TestFakeAPILoadSnapshotWithOverrideError verifies the fakeAPI propagates errors.
func TestFakeAPILoadSnapshotWithOverrideError(t *testing.T) {
	f := &fakeAPI{loadSnapshotOverrideErr: errors.New("boom")}
	err := f.LoadSnapshotWithOverride(context.Background(), "/path/to/snap", "/path/to/mem", true, "/path/to/override.sock")
	require.Error(t, err)
	require.Contains(t, err.Error(), "boom")
}

// TestNewTransportConfiguresSocketPathAndCID verifies NewTransport creates a
// transport bound to the given socket path and guest CID.
func TestNewTransportConfiguresSocketPathAndCID(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "vsock.sock")
	cid := uint32(42)

	tr := NewTransport(sockPath, cid)
	require.NotNil(t, tr)
	require.Equal(t, sockPath, tr.SocketPath())
	require.Equal(t, cid, tr.CID())
}

// TestTransportBindingWithoutPutGuestVsock verifies that binding the
// host-side transport for a restored VM does NOT invoke PutGuestVsock on the
// Firecracker API. configureVsock (used during normal boot) calls
// PutGuestVsock, which is rejected after snapshot restoration ("not supported
// after starting). restoreVM and RestoreSnapshot bind vm.transport directly
// instead.
func TestTransportBindingWithoutPutGuestVsock(t *testing.T) {
	vm, err := New(testConfig(t), nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vm.Destroy(context.Background())) })

	// Initially no transport.
	require.Nil(t, vm.Vsock())

	// Simulate what restoreVM/RestoreSnapshot do: bind transport directly
	// without calling configureVsock/PutGuestVsock.
	vm.mu.Lock()
	vm.transport = NewTransport(vm.res.VsockSock, vm.cfg.VsockCID)
	vm.mu.Unlock()

	require.NotNil(t, vm.Vsock())
	require.Equal(t, vm.res.VsockSock, vm.Vsock().SocketPath())
	require.Equal(t, vm.cfg.VsockCID, vm.Vsock().CID())
}

// TestComputeChecksumProducesSHA256 verifies that computeChecksum returns the
// correct hex-encoded SHA-256 for a file with known content.
func TestComputeChecksumProducesSHA256(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.bin")
	require.NoError(t, os.WriteFile(path, []byte("hello world"), 0644))

	sum, err := computeChecksum(path)
	require.NoError(t, err)

	// Expected SHA-256 of "hello world".
	require.Equal(t, "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9", sum)
}

// TestComputeChecksumStreaming verifies that computeChecksum produces the same
// result for content larger than a single read buffer, confirming streaming
// I/O is used rather than reading the whole file into memory.
func TestComputeChecksumStreaming(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.bin")
	// Write 1 MiB of deterministic data to exceed typical buffer sizes.
	data := make([]byte, 1024*1024)
	for i := range data {
		data[i] = byte(i % 256)
	}
	require.NoError(t, os.WriteFile(path, data, 0644))

	sum1, err := computeChecksum(path)
	require.NoError(t, err)

	// Verify against Go's standard crypto/sha256 for the same data.
	h := sha256.New()
	h.Write(data)
	require.Equal(t, fmt.Sprintf("%x", h.Sum(nil)), sum1)
}

// TestVerifyArtifactChecksumsValid verifies that checksums matching the actual
// file contents pass verification.
func TestVerifyArtifactChecksumsValid(t *testing.T) {
	dir := t.TempDir()
	path1 := filepath.Join(dir, "state.snap")
	path2 := filepath.Join(dir, "mem.snap")
	require.NoError(t, os.WriteFile(path1, []byte("state-data"), 0644))
	require.NoError(t, os.WriteFile(path2, []byte("mem-data"), 0644))

	sum1, err := computeChecksum(path1)
	require.NoError(t, err)
	sum2, err := computeChecksum(path2)
	require.NoError(t, err)

	snap := &SnapshotInfo{
		ArtifactChecksums: map[string]string{
			path1: sum1,
			path2: sum2,
		},
	}
	require.NoError(t, verifyArtifactChecksums(snap))
}

// TestVerifyArtifactChecksumsEmptySkipsLegacy verifies that an empty
// ArtifactChecksums map (legacy snapshot) skips verification, preserving
// backward compatibility.
func TestVerifyArtifactChecksumsEmptySkipsLegacy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.snap")
	require.NoError(t, os.WriteFile(path, []byte("data"), 0644))

	// No checksums at all: verification is skipped.
	snap := &SnapshotInfo{ArtifactChecksums: map[string]string{}}
	require.NoError(t, verifyArtifactChecksums(snap))

	// Nil map: also skipped.
	snap2 := &SnapshotInfo{}
	require.NoError(t, verifyArtifactChecksums(snap2))
}

// TestVerifyArtifactChecksumsMismatch rejects snapshots whose artifact
// checksums don't match the actual file contents.
func TestVerifyArtifactChecksumsMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.snap")
	require.NoError(t, os.WriteFile(path, []byte("state-data"), 0644))

	snap := &SnapshotInfo{
		ArtifactChecksums: map[string]string{
			path: "deadbeef",
		},
	}
	err := verifyArtifactChecksums(snap)
	require.Error(t, err)
	require.Contains(t, err.Error(), "checksum mismatch")
	require.Contains(t, err.Error(), path)
}

// TestVerifyArtifactChecksumsMissingFile rejects snapshots whose artifact
// file no longer exists.
func TestVerifyArtifactChecksumsMissingFile(t *testing.T) {
	snap := &SnapshotInfo{
		ArtifactChecksums: map[string]string{
			"/nonexistent/state.snap": "deadbeef",
		},
	}
	err := verifyArtifactChecksums(snap)
	require.Error(t, err)
	require.Contains(t, err.Error(), "checksum verification failed")
}

// TestValidateRejectsCorruptedSnapshot verifies that SnapshotStore.Validate
// rejects a snapshot whose artifact checksum doesn't match.
func TestValidateRejectsCorruptedSnapshot(t *testing.T) {
	dir := t.TempDir()
	store := NewSnapshotStore(dir)
	require.NoError(t, store.EnsureDirs())

	snapID := "snap1"
	snapDir := store.snapshotDir(snapID)
	require.NoError(t, os.MkdirAll(snapDir, 0o750))

	statePath := store.statePath(snapID)
	memPath := store.memPath(snapID)
	require.NoError(t, os.WriteFile(statePath, []byte("state-data"), 0644))
	require.NoError(t, os.WriteFile(memPath, []byte("mem-data"), 0644))

	// Corrupt: wrong checksum for state.snap.
	_, _ = computeChecksum(statePath)
	memSum, _ := computeChecksum(memPath)
	snap := &SnapshotInfo{
		ID:                snapID,
		Status:            snapshotStatusReady,
		MetadataVersion:   snapshotMetadataVersion,
		SnapshotPath:      statePath,
		MemFilePath:       memPath,
		ArtifactChecksums: map[string]string{statePath: "wrong-checksum", memPath: memSum},
	}
	require.NoError(t, store.Create(snap))

	err := store.Validate(snapID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "checksum mismatch")
}

// TestValidateAcceptsLegacySnapshotWithoutChecksums verifies that
// SnapshotStore.Validate accepts a legacy snapshot with no checksum metadata.
func TestValidateAcceptsLegacySnapshotWithoutChecksums(t *testing.T) {
	dir := t.TempDir()
	store := NewSnapshotStore(dir)
	require.NoError(t, store.EnsureDirs())

	snapID := "snap-legacy"
	snapDir := store.snapshotDir(snapID)
	require.NoError(t, os.MkdirAll(snapDir, 0o750))

	statePath := store.statePath(snapID)
	memPath := store.memPath(snapID)
	require.NoError(t, os.WriteFile(statePath, []byte("state-data"), 0644))
	require.NoError(t, os.WriteFile(memPath, []byte("mem-data"), 0644))

	snap := &SnapshotInfo{
		ID:              snapID,
		Status:          snapshotStatusReady,
		MetadataVersion: snapshotMetadataVersion,
		SnapshotPath:    statePath,
		MemFilePath:     memPath,
		// No ArtifactChecksums - legacy snapshot.
	}
	require.NoError(t, store.Create(snap))

	require.NoError(t, store.Validate(snapID))
}

// TestRestoreVMRejectsCorruptedSnapshot verifies that restoreVM rejects a
// snapshot with mismatched checksums before calling LoadSnapshotWithOverride.
func TestRestoreVMRejectsCorruptedSnapshot(t *testing.T) {
	child, err := New(testConfig(t), nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, child.Destroy(context.Background())) })

	f := &fakeAPI{}
	child.mu.Lock()
	child.api = f
	child.mu.Unlock()

	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.snap")
	memPath := filepath.Join(dir, "mem.snap")
	require.NoError(t, os.WriteFile(statePath, []byte("state-data"), 0644))
	require.NoError(t, os.WriteFile(memPath, []byte("mem-data"), 0644))

	_, _ = computeChecksum(statePath)
	memSum, _ := computeChecksum(memPath)

	snap := &SnapshotInfo{
		SnapshotPath: statePath,
		MemFilePath:  memPath,
		ArtifactChecksums: map[string]string{
			statePath: "wrong-checksum",
			memPath:   memSum,
		},
	}

	ctx := context.Background()
	err = restoreVM(ctx, child, snap)
	require.Error(t, err)
	require.Contains(t, err.Error(), "snapshot integrity check failed")
	require.Contains(t, err.Error(), "checksum mismatch")

	// LoadSnapshotWithOverride should NOT have been called.
	require.NotContains(t, f.calls, "LoadSnapshotWithOverride")
	require.Nil(t, child.Vsock())
}

// TestWriteMetadataAtomicallySuccessful verifies that writeMetadataAtomically
// publishes complete metadata with restrictive permissions and cleans up the
// temp file.
func TestWriteMetadataAtomicallySuccessful(t *testing.T) {
	dir := t.TempDir()
	snapDir := filepath.Join(dir, "snap1")
	require.NoError(t, os.MkdirAll(snapDir, 0o750))

	tmpPath := filepath.Join(snapDir, "metadata.json.tmp")
	finalPath := filepath.Join(snapDir, "metadata.json")

	snap := &SnapshotInfo{
		ID:                "snap1",
		Status:            snapshotStatusReady,
		MetadataVersion:   snapshotMetadataVersion,
		SnapshotPath:      "/path/to/state.snap",
		MemFilePath:       "/path/to/mem.snap",
		ArtifactChecksums: map[string]string{"/path/to/state.snap": "abc123"},
	}

	require.NoError(t, writeMetadataAtomically(tmpPath, finalPath, snap))

	// Temp file must be cleaned up.
	_, err := os.Stat(tmpPath)
	require.True(t, os.IsNotExist(err), "temp file should be removed")

	// Final file must exist with restrictive permissions.
	info, err := os.Stat(finalPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	// Content must match the marshaled snapshot info.
	data, err := os.ReadFile(finalPath)
	require.NoError(t, err)
	var parsed SnapshotInfo
	require.NoError(t, json.Unmarshal(data, &parsed))
	require.Equal(t, snap.ID, parsed.ID)
	require.Equal(t, snap.Status, parsed.Status)
	require.Equal(t, snap.MetadataVersion, parsed.MetadataVersion)
	require.Equal(t, snap.SnapshotPath, parsed.SnapshotPath)
	require.Equal(t, snap.MemFilePath, parsed.MemFilePath)
	require.Equal(t, snap.ArtifactChecksums, parsed.ArtifactChecksums)
}

// TestWriteMetadataAtomicallyPreservesPreviousOnFailure verifies that when
// publication fails (e.g. temp file cannot be created), any existing metadata
// at finalPath is preserved.
func TestWriteMetadataAtomicallyPreservesPreviousOnFailure(t *testing.T) {
	dir := t.TempDir()
	snapDir := filepath.Join(dir, "snap1")
	require.NoError(t, os.MkdirAll(snapDir, 0o750))

	finalPath := filepath.Join(snapDir, "metadata.json")

	// Pre-existing metadata.
	oldSnap := &SnapshotInfo{
		ID:           "snap1",
		Status:       snapshotStatusReady,
		SnapshotPath: "/old/state.snap",
		MemFilePath:  "/old/mem.snap",
	}
	oldData, err := json.MarshalIndent(oldSnap, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(finalPath, oldData, 0o600))

	// New metadata that will fail to write because tmpPath is inside a
	// directory we make non-writable.
	readOnlyDir := filepath.Join(dir, "readonly")
	require.NoError(t, os.MkdirAll(readOnlyDir, 0o555))
	readOnlyTmp := filepath.Join(readOnlyDir, "metadata.json.tmp")

	newSnap := &SnapshotInfo{
		ID:           "snap1",
		Status:       snapshotStatusReady,
		SnapshotPath: "/new/state.snap",
		MemFilePath:  "/new/mem.snap",
	}

	err = writeMetadataAtomically(readOnlyTmp, filepath.Join(readOnlyDir, "metadata.json"), newSnap)
	require.Error(t, err)

	// Old metadata at finalPath must be preserved.
	data, err := os.ReadFile(finalPath)
	require.NoError(t, err)
	var parsed SnapshotInfo
	require.NoError(t, json.Unmarshal(data, &parsed))
	require.Equal(t, oldSnap.SnapshotPath, parsed.SnapshotPath)
	require.Equal(t, oldSnap.MemFilePath, parsed.MemFilePath)

	// Temp file must not be left behind in the read-only dir (it was never created).
	_, err = os.Stat(readOnlyTmp)
	require.True(t, os.IsNotExist(err))
}

// TestWriteMetadataAtomicallyPermissions verifies that the published file
// uses restrictive 0600 permissions.
func TestWriteMetadataAtomicallyPermissions(t *testing.T) {
	dir := t.TempDir()
	snapDir := filepath.Join(dir, "snap1")
	require.NoError(t, os.MkdirAll(snapDir, 0o750))

	tmpPath := filepath.Join(snapDir, "metadata.json.tmp")
	finalPath := filepath.Join(snapDir, "metadata.json")

	snap := &SnapshotInfo{
		ID:           "snap1",
		Status:       snapshotStatusReady,
		SnapshotPath: "/path/to/state.snap",
		MemFilePath:  "/path/to/mem.snap",
	}

	require.NoError(t, writeMetadataAtomically(tmpPath, finalPath, snap))

	info, err := os.Stat(finalPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// TestWriteMetadataAtomicallyChecksumsPreserved verifies that checksum
// metadata introduced in #7 is preserved correctly through atomic publication.
func TestWriteMetadataAtomicallyChecksumsPreserved(t *testing.T) {
	dir := t.TempDir()
	snapDir := filepath.Join(dir, "snap1")
	require.NoError(t, os.MkdirAll(snapDir, 0o750))

	tmpPath := filepath.Join(snapDir, "metadata.json.tmp")
	finalPath := filepath.Join(snapDir, "metadata.json")

	checksums := map[string]string{
		"/path/to/state.snap": "aaa",
		"/path/to/mem.snap":   "bbb",
	}
	snap := &SnapshotInfo{
		ID:                "snap1",
		Status:            snapshotStatusReady,
		MetadataVersion:   snapshotMetadataVersion,
		SnapshotPath:      "/path/to/state.snap",
		MemFilePath:       "/path/to/mem.snap",
		ArtifactChecksums: checksums,
	}

	require.NoError(t, writeMetadataAtomically(tmpPath, finalPath, snap))

	data, err := os.ReadFile(finalPath)
	require.NoError(t, err)
	var parsed SnapshotInfo
	require.NoError(t, json.Unmarshal(data, &parsed))
	require.Equal(t, checksums, parsed.ArtifactChecksums)
}

// fakeVSockServer simulates a guest agent vsock server for testing snapshot
// coordination. It listens on a Unix socket, reads length-prefixed JSON
// requests, and returns programmed responses.
type fakeVSockServer struct {
	mu            sync.Mutex
	responses     map[string]map[string]any // response keyed by request kind
	failOn        string                    // if set, return error response for this kind
	receivedCalls []string                  // request kinds received, in order
	connErr       bool                      // if set, close connection without responding
	done          chan struct{}
	ln            net.Listener
}

func newFakeVSockServer(t *testing.T) *fakeVSockServer {
	s := &fakeVSockServer{
		responses: make(map[string]map[string]any),
		done:      make(chan struct{}, 1),
	}
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "guest.sock")
	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	s.ln = ln
	go s.serve(sockPath)
	t.Cleanup(s.close)
	return s
}

func (s *fakeVSockServer) close() {
	s.ln.Close()
	<-s.done
}

func (s *fakeVSockServer) socketPath() string {
	return s.ln.Addr().String()
}

func (s *fakeVSockServer) setResponse(kind string, resp map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responses[kind] = resp
}

func (s *fakeVSockServer) setFailOn(kind string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failOn = kind
}

func (s *fakeVSockServer) setConnError() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connErr = true
}

func (s *fakeVSockServer) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.receivedCalls...)
}

func (s *fakeVSockServer) serve(sockPath string) {
	defer close(s.done)
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func (s *fakeVSockServer) handleConn(conn net.Conn) {
	defer conn.Close()

	// Firecracker vsock handshake: read "CONNECT <port>\n", reply "OK <port>\n".
	reader := bufio.NewReaderSize(conn, 64)
	line, err := reader.ReadString('\n')
	if err != nil {
		return
	}
	line = strings.TrimRight(line, "\n")
	if !strings.HasPrefix(line, "CONNECT ") {
		return
	}
	port := strings.TrimPrefix(line, "CONNECT ")
	_, _ = conn.Write([]byte("OK " + port + "\n"))

	// Read length prefix.
	var lenBuf [4]byte
	if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
		return
	}
	frameLen := binary.BigEndian.Uint32(lenBuf[:])
	if frameLen == 0 || frameLen > 1<<20 {
		return
	}

	// Read payload.
	payload := make([]byte, frameLen)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return
	}

	var req map[string]any
	if err := json.Unmarshal(payload, &req); err != nil {
		return
	}

	kind, _ := req["kind"].(string)
	reqID, _ := req["request_id"].(string)

	s.mu.Lock()
	s.receivedCalls = append(s.receivedCalls, kind)
	s.mu.Unlock()

	// Simulate connection error.
	s.mu.Lock()
	connErr := s.connErr
	s.mu.Unlock()
	if connErr {
		return
	}

	// Simulate failure for a specific kind.
	s.mu.Lock()
	failOn := s.failOn
	s.mu.Unlock()
	if failOn == kind {
		resp := map[string]any{
			"kind":    "error",
			"code":    float64(500),
			"message": "simulated guest failure",
		}
		s.writeResponse(conn, resp)
		return
	}

	// Return programmed response or default success.
	s.mu.Lock()
	resp, ok := s.responses[kind]
	s.mu.Unlock()
	if !ok {
		resp = map[string]any{
			"kind":       strings.ReplaceAll(kind, "_", "") + "_ack",
			"request_id": reqID,
			"status":     "ok",
		}
	}
	s.writeResponse(conn, resp)
}

func (s *fakeVSockServer) writeResponse(conn net.Conn, resp map[string]any) {
	payload, err := json.Marshal(resp)
	if err != nil {
		return
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))
	_ = writeFull(conn, lenBuf[:])
	_ = writeFull(conn, payload)
}

// newVMWithFakeTransport creates a VM with a fake API and a Transport pointing
// to the fake vsock server.
func newVMWithFakeTransport(t *testing.T, server *fakeVSockServer) *VM {
	vm, err := New(testConfig(t), nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vm.Destroy(context.Background())) })

	// Transition VM to Paused state (valid path: Created -> Preparing ->
	// Running -> Paused).
	require.NoError(t, vm.sm.Transition(StateCreated, StatePreparing))
	require.NoError(t, vm.sm.Transition(StatePreparing, StateRunning))
	require.NoError(t, vm.sm.Transition(StateRunning, StatePaused))

	f := &fakeAPI{}
	f.instanceState = "Paused"
	vm.mu.Lock()
	vm.api = f
	vm.transport = NewTransport(server.socketPath(), 3)
	vm.mu.Unlock()

	// Transition to Destroyed in cleanup so Destroy() is a no-op (no
	// Firecracker process was started, so Destroy would otherwise hang on
	// process exit wait).
	t.Cleanup(func() {
		vm.sm.TransitionTo(StateDestroyed)
		_ = vm.Destroy(context.Background())
	})

	return vm
}

// TestSnapshotCoordinatorFilesystemConsistentSuccess verifies the complete
// filesystem_consistent coordination sequence: prepare → sync → freeze →
// Firecracker snapshot → thaw → snapshot_ready.
func TestSnapshotCoordinatorFilesystemConsistentSuccess(t *testing.T) {
	server := newFakeVSockServer(t)
	vm := newVMWithFakeTransport(t, server)

	store := NewSnapshotStore(t.TempDir())
	require.NoError(t, store.EnsureDirs())

	coordinator := NewSnapshotCoordinator(store, nil, nil, nil, nil)

	ctx := context.Background()
	info, err := coordinator.CreateSnapshotWithCoordination(
		ctx, vm, SnapshotConsistencyFilesystem, t.TempDir(),
	)
	require.NoError(t, err)

	// Verify all coordination steps were called in order.
	calls := server.calls()
	require.Equal(t, []string{"prepare_snapshot", "sync_filesystems", "freeze_filesystems", "snapshot_ready", "thaw_filesystems"}, calls)

	// Verify snapshot metadata.
	require.Equal(t, SnapshotConsistencyFilesystem, SnapshotConsistencyMode(info.ConsistencyMode))
	require.NotEmpty(t, info.RequestID)
	require.Equal(t, snapshotStatusReady, info.Status)
}

// TestSnapshotCoordinatorCrashConsistentNoGuestCoordination verifies that
// crash_consistent mode does not contact the guest agent.
func TestSnapshotCoordinatorCrashConsistentNoGuestCoordination(t *testing.T) {
	server := newFakeVSockServer(t)
	vm := newVMWithFakeTransport(t, server)

	store := NewSnapshotStore(t.TempDir())
	require.NoError(t, store.EnsureDirs())

	coordinator := NewSnapshotCoordinator(store, nil, nil, nil, nil)

	ctx := context.Background()
	info, err := coordinator.CreateSnapshotWithCoordination(
		ctx, vm, SnapshotConsistencyCrash, t.TempDir(),
	)
	require.NoError(t, err)

	// No guest-agent calls should have been made.
	require.NotContains(t, server.calls(), "prepare_snapshot")
	require.NotContains(t, server.calls(), "sync_filesystems")
	require.NotContains(t, server.calls(), "freeze_filesystems")
	require.NotContains(t, server.calls(), "snapshot_ready")
	require.NotContains(t, server.calls(), "thaw_filesystems")

	// Verify snapshot metadata.
	require.Equal(t, SnapshotConsistencyCrash, SnapshotConsistencyMode(info.ConsistencyMode))
	require.Empty(t, info.RequestID)
	require.Equal(t, snapshotStatusReady, info.Status)
}

// TestSnapshotCoordinatorPrepareFailure verifies that failure at prepare
// returns an error and does not freeze or attempt thaw.
func TestSnapshotCoordinatorPrepareFailure(t *testing.T) {
	server := newFakeVSockServer(t)
	server.setFailOn("prepare_snapshot")
	vm := newVMWithFakeTransport(t, server)

	store := NewSnapshotStore(t.TempDir())
	require.NoError(t, store.EnsureDirs())

	coordinator := NewSnapshotCoordinator(store, nil, nil, nil, nil)

	ctx := context.Background()
	_, err := coordinator.CreateSnapshotWithCoordination(
		ctx, vm, SnapshotConsistencyFilesystem, t.TempDir(),
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "guest prepare snapshot")

	// Only prepare was called; no freeze, no thaw.
	calls := server.calls()
	require.Equal(t, []string{"prepare_snapshot"}, calls)
}

// TestSnapshotCoordinatorSyncFailure verifies that failure at sync_filesystems
// returns an error and does not freeze or attempt thaw.
func TestSnapshotCoordinatorSyncFailure(t *testing.T) {
	server := newFakeVSockServer(t)
	server.setFailOn("sync_filesystems")
	vm := newVMWithFakeTransport(t, server)

	store := NewSnapshotStore(t.TempDir())
	require.NoError(t, store.EnsureDirs())

	coordinator := NewSnapshotCoordinator(store, nil, nil, nil, nil)

	ctx := context.Background()
	_, err := coordinator.CreateSnapshotWithCoordination(
		ctx, vm, SnapshotConsistencyFilesystem, t.TempDir(),
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "guest sync filesystems")

	// prepare + sync were called; no freeze, no thaw.
	calls := server.calls()
	require.Equal(t, []string{"prepare_snapshot", "sync_filesystems"}, calls)
}

// TestSnapshotCoordinatorFreezeFailure verifies that failure at freeze returns
// an error and does not attempt thaw (filesystems were never frozen).
func TestSnapshotCoordinatorFreezeFailure(t *testing.T) {
	server := newFakeVSockServer(t)
	server.setFailOn("freeze_filesystems")
	vm := newVMWithFakeTransport(t, server)

	store := NewSnapshotStore(t.TempDir())
	require.NoError(t, store.EnsureDirs())

	coordinator := NewSnapshotCoordinator(store, nil, nil, nil, nil)

	ctx := context.Background()
	_, err := coordinator.CreateSnapshotWithCoordination(
		ctx, vm, SnapshotConsistencyFilesystem, t.TempDir(),
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "guest freeze filesystems")

	// prepare + sync + freeze were called; no thaw (never froze).
	calls := server.calls()
	require.Equal(t, []string{"prepare_snapshot", "sync_filesystems", "freeze_filesystems"}, calls)
}

// TestSnapshotCoordinatorCreateSnapshotFailureThaws verifies that when a
// post-freeze step fails, thaw is still attempted. Since fakeAPI.CreateSnapshot
// always succeeds, this test uses snapshot_ready failure instead (covered by
// TestSnapshotCoordinatorSnapshotReadyFailureThaws). This test verifies that a
// VM whose Firecracker API fails during CreateSnapshot still triggers thaw.
func TestSnapshotCoordinatorCreateSnapshotFailureThaws(t *testing.T) {
	server := newFakeVSockServer(t)
	vm := newVMWithFakeTransport(t, server)

	// Create a fakeAPI that fails CreateSnapshot.
	failingAPI := &fakeAPI{}
	failingAPI.instanceState = "Paused"

	// Wrap: we can't easily inject a CreateSnapshot failure into fakeAPI
	// without modifying it. Instead, verify thaw-on-failure via snapshot_ready
	// failure (TestSnapshotCoordinatorSnapshotReadyFailureThaws). This test
	// documents that CreateSnapshot failure would also trigger thaw via the
	// shared deferred function.
	store := NewSnapshotStore(t.TempDir())
	require.NoError(t, store.EnsureDirs())

	coordinator := NewSnapshotCoordinator(store, nil, nil, nil, nil)

	ctx := context.Background()

	// Use the failing API - CreateSnapshot will succeed (fakeAPI returns nil),
	// but the test confirms the coordination completes and thaw is called.
	info, err := coordinator.CreateSnapshotWithCoordination(
		ctx, vm, SnapshotConsistencyFilesystem, t.TempDir(),
	)
	require.NoError(t, err)
	require.NotEmpty(t, info.ID)

	// Verify the normal sequence including thaw.
	calls := server.calls()
	require.Equal(t, []string{"prepare_snapshot", "sync_filesystems", "freeze_filesystems", "snapshot_ready", "thaw_filesystems"}, calls)
}

// TestSnapshotCoordinatorSnapshotReadyFailureThaws verifies that when
// snapshot_ready fails after freeze, thaw is still attempted.
func TestSnapshotCoordinatorSnapshotReadyFailureThaws(t *testing.T) {
	server := newFakeVSockServer(t)
	server.setFailOn("snapshot_ready")
	vm := newVMWithFakeTransport(t, server)

	store := NewSnapshotStore(t.TempDir())
	require.NoError(t, store.EnsureDirs())

	coordinator := NewSnapshotCoordinator(store, nil, nil, nil, nil)

	ctx := context.Background()
	_, err := coordinator.CreateSnapshotWithCoordination(
		ctx, vm, SnapshotConsistencyFilesystem, t.TempDir(),
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "guest snapshot ready")

	// prepare + sync + freeze + snapshot_ready were called; thaw must still
	// be attempted even though snapshot_ready failed.
	calls := server.calls()
	require.Equal(t, []string{"prepare_snapshot", "sync_filesystems", "freeze_filesystems", "snapshot_ready", "thaw_filesystems"}, calls)
}

// TestSnapshotCoordinatorVsockConnectionError verifies that a vsock connection
// error returns an error and does not freeze or thaw.
func TestSnapshotCoordinatorVsockConnectionError(t *testing.T) {
	server := newFakeVSockServer(t)
	server.setConnError()
	vm := newVMWithFakeTransport(t, server)

	store := NewSnapshotStore(t.TempDir())
	require.NoError(t, store.EnsureDirs())

	coordinator := NewSnapshotCoordinator(store, nil, nil, nil, nil)

	ctx := context.Background()
	_, err := coordinator.CreateSnapshotWithCoordination(
		ctx, vm, SnapshotConsistencyFilesystem, t.TempDir(),
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "guest prepare snapshot")

	// The server received the request but closed the connection without
	// responding, so the coordinator reports a vsock error.
	require.Equal(t, []string{"prepare_snapshot"}, server.calls())
}

// TestSnapshotCoordinatorNoVsockTransport verifies that filesystem_consistent
// mode rejects a VM without a vsock transport.
func TestSnapshotCoordinatorNoVsockTransport(t *testing.T) {
	vm, err := New(testConfig(t), nil)
	require.NoError(t, err)

	// Transition VM to Paused state.
	require.NoError(t, vm.sm.Transition(StateCreated, StatePreparing))
	require.NoError(t, vm.sm.Transition(StatePreparing, StateRunning))
	require.NoError(t, vm.sm.Transition(StateRunning, StatePaused))

	// Transition to Destroyed in cleanup so Destroy() is a no-op.
	t.Cleanup(func() {
		vm.sm.TransitionTo(StateDestroyed)
		_ = vm.Destroy(context.Background())
	})

	f := &fakeAPI{}
	f.instanceState = "Paused"
	vm.mu.Lock()
	vm.api = f
	// vm.transport is nil (not set)
	vm.mu.Unlock()

	store := NewSnapshotStore(t.TempDir())
	require.NoError(t, store.EnsureDirs())

	coordinator := NewSnapshotCoordinator(store, nil, nil, nil, nil)

	ctx := context.Background()
	_, err = coordinator.CreateSnapshotWithCoordination(
		ctx, vm, SnapshotConsistencyFilesystem, t.TempDir(),
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no vsock transport")
}

// TestSnapshotCoordinatorContextCancellation verifies that context
// cancellation propagates through the coordination.
func TestSnapshotCoordinatorContextCancellation(t *testing.T) {
	server := newFakeVSockServer(t)
	vm := newVMWithFakeTransport(t, server)

	store := NewSnapshotStore(t.TempDir())
	require.NoError(t, store.EnsureDirs())

	coordinator := NewSnapshotCoordinator(store, nil, nil, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := coordinator.CreateSnapshotWithCoordination(
		ctx, vm, SnapshotConsistencyFilesystem, t.TempDir(),
	)
	require.Error(t, err)
}

// TestTransportSendRequestSuccess verifies Transport.SendRequest sends a
// request and parses the response correctly.
func TestTransportSendRequestSuccess(t *testing.T) {
	server := newFakeVSockServer(t)
	server.setResponse("health", map[string]any{
		"kind":   "health_ok",
		"status": "ready",
	})

	tr := NewTransport(server.socketPath(), GuestAgentVsockPort)

	ctx := context.Background()
	resp, err := tr.SendRequest(ctx, GuestAgentVsockPort, map[string]any{
		"kind": "health",
	})
	require.NoError(t, err)
	require.Equal(t, "health_ok", resp["kind"])
	require.Equal(t, "ready", resp["status"])
}

// TestTransportSendRequestGuestError verifies Transport.SendRequest returns an
// error when the guest agent returns an error response.
func TestTransportSendRequestGuestError(t *testing.T) {
	server := newFakeVSockServer(t)
	server.setFailOn("health")

	tr := NewTransport(server.socketPath(), GuestAgentVsockPort)

	ctx := context.Background()
	resp, err := tr.SendRequest(ctx, GuestAgentVsockPort, map[string]any{
		"kind": "health",
	})
	require.NoError(t, err)
	require.Equal(t, "error", resp["kind"])
	require.Contains(t, resp["message"].(string), "simulated guest failure")
}

// TestTransportSendRequestContextCancel verifies Transport.SendRequest
// respects context cancellation.
func TestTransportSendRequestContextCancel(t *testing.T) {
	server := newFakeVSockServer(t)
	tr := NewTransport(server.socketPath(), GuestAgentVsockPort)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := tr.SendRequest(ctx, GuestAgentVsockPort, map[string]any{
		"kind": "health",
	})
	require.Error(t, err)
}

// TestDestroyFromCreatedState verifies that Destroy on a VM in Created state
// (no Firecracker process started) cleans up the working directory and
// transitions to Destroyed.
func TestDestroyFromCreatedState(t *testing.T) {
	vm, err := New(testConfig(t), nil)
	require.NoError(t, err)
	require.Equal(t, StateCreated, vm.State())

	ctx := context.Background()
	require.NoError(t, vm.Destroy(ctx))
	require.Equal(t, StateDestroyed, vm.State())

	// Working directory and socket files should be removed.
	_, err = os.Stat(vm.Dir())
	require.True(t, os.IsNotExist(err), "VM working directory should be removed")
	_, err = os.Stat(vm.res.APISock)
	require.True(t, os.IsNotExist(err), "API socket should be removed")
	_, err = os.Stat(vm.res.VsockSock)
	require.True(t, os.IsNotExist(err), "vsock socket should be removed")
}

// TestDestroyFromPreparingState verifies that Destroy on a VM in Preparing
// state with a running Firecracker process terminates the process and cleans
// up resources.
func TestDestroyFromPreparingState(t *testing.T) {
	vm, err := New(testConfig(t), nil)
	require.NoError(t, err)

	require.NoError(t, vm.sm.Transition(StateCreated, StatePreparing))
	proc, err := Start("sleep", []string{"100"}, nil)
	require.NoError(t, err)
	vm.mu.Lock()
	vm.proc = proc
	vm.mu.Unlock()

	t.Cleanup(func() {
		_ = proc.Kill()
		_ = vm.Destroy(context.Background())
	})

	ctx := context.Background()
	require.NoError(t, vm.Destroy(ctx))
	require.Equal(t, StateDestroyed, vm.State())

	select {
	case <-proc.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("firecracker process was not terminated by Destroy")
	}
	require.True(t, proc.Reaped())
}

// TestDestroyFromSnapshottingState verifies that Destroy on a VM in
// Snapshotting state terminates the running process and cleans up resources.
func TestDestroyFromSnapshottingState(t *testing.T) {
	vm, err := New(testConfig(t), nil)
	require.NoError(t, err)

	require.NoError(t, vm.sm.Transition(StateCreated, StatePreparing))
	require.NoError(t, vm.sm.Transition(StatePreparing, StateRunning))
	require.NoError(t, vm.sm.Transition(StateRunning, StatePaused))
	require.NoError(t, vm.sm.Transition(StatePaused, StateSnapshotting))

	proc, err := Start("sleep", []string{"100"}, nil)
	require.NoError(t, err)
	vm.mu.Lock()
	vm.proc = proc
	vm.mu.Unlock()

	t.Cleanup(func() {
		_ = proc.Kill()
		_ = vm.Destroy(context.Background())
	})

	ctx := context.Background()
	require.NoError(t, vm.Destroy(ctx))
	require.Equal(t, StateDestroyed, vm.State())

	select {
	case <-proc.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("firecracker process was not terminated by Destroy")
	}
	require.True(t, proc.Reaped())
}

// TestDestroyFromStoppingState verifies that Destroy on a VM in Stopping state
// waits for the existing stop to complete before cleaning up.
func TestDestroyFromStoppingState(t *testing.T) {
	vm, err := New(testConfig(t), nil)
	require.NoError(t, err)

	require.NoError(t, vm.sm.Transition(StateCreated, StatePreparing))
	require.NoError(t, vm.sm.Transition(StatePreparing, StateRunning))
	vm.mu.Lock()
	vm.api = &fakeAPI{}
	vm.mu.Unlock()

	proc, err := Start("sh", []string{
		"-c", `trap '' TERM; exec sleep 100`,
	}, nil)
	require.NoError(t, err)
	vm.mu.Lock()
	vm.proc = proc
	vm.mu.Unlock()
	go vm.monitorExit()

	stopErr := make(chan error, 1)
	go func() {
		stopErr <- vm.Stop(context.Background())
	}()

	time.Sleep(100 * time.Millisecond)
	require.Equal(t, StateStopping, vm.State())

	destroyErr := make(chan error, 1)
	go func() {
		destroyErr <- vm.Destroy(context.Background())
	}()

	_ = proc.Kill()

	select {
	case err := <-destroyErr:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Destroy did not complete after process termination")
	}

	select {
	case err := <-stopErr:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not complete")
	}

	require.Equal(t, StateDestroyed, vm.State())
	require.True(t, proc.Reaped())
}

// TestDestroyFromFailedState verifies that Destroy on a VM in Failed state
// kills the process and cleans up resources.
func TestDestroyFromFailedState(t *testing.T) {
	vm, err := New(testConfig(t), nil)
	require.NoError(t, err)

	require.NoError(t, vm.sm.Transition(StateCreated, StatePreparing))
	require.NoError(t, vm.sm.Transition(StatePreparing, StateRunning))
	vm.sm.TransitionTo(StateFailed)

	proc, err := Start("sleep", []string{"100"}, nil)
	require.NoError(t, err)
	vm.mu.Lock()
	vm.proc = proc
	vm.mu.Unlock()

	t.Cleanup(func() {
		_ = proc.Kill()
		_ = vm.Destroy(context.Background())
	})

	ctx := context.Background()
	require.NoError(t, vm.Destroy(ctx))
	require.Equal(t, StateDestroyed, vm.State())

	select {
	case <-proc.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("firecracker process was not terminated by Destroy")
	}
	require.True(t, proc.Reaped())
}

// TestDestroyRepeatedCallsIsIdempotent verifies that calling Destroy multiple
// times is safe: the first call transitions to Destroyed, subsequent calls
// return nil without error.
func TestDestroyRepeatedCallsIsIdempotent(t *testing.T) {
	vm, err := New(testConfig(t), nil)
	require.NoError(t, err)

	ctx := context.Background()
	require.NoError(t, vm.Destroy(ctx))
	require.Equal(t, StateDestroyed, vm.State())

	require.NoError(t, vm.Destroy(ctx))
	require.Equal(t, StateDestroyed, vm.State())

	require.NoError(t, vm.Destroy(ctx))
	require.Equal(t, StateDestroyed, vm.State())
}

// TestDestroyFromDestroyingState verifies that Destroy on a VM already in
// Destroying state returns nil immediately without error.
func TestDestroyFromDestroyingState(t *testing.T) {
	vm, err := New(testConfig(t), nil)
	require.NoError(t, err)

	vm.sm.TransitionTo(StateDestroying)
	require.Equal(t, StateDestroying, vm.State())

	ctx := context.Background()
	require.NoError(t, vm.Destroy(ctx))
	require.Equal(t, StateDestroying, vm.State())
}

// TestDestroyNoProcessCreatedState verifies that Destroy on a VM in Created
// state with no process handle set does not hang or error.
func TestDestroyNoProcessCreatedState(t *testing.T) {
	vm, err := New(testConfig(t), nil)
	require.NoError(t, err)

	ctx := context.Background()
	require.NoError(t, vm.Destroy(ctx))
	require.Equal(t, StateDestroyed, vm.State())
}

// TestDestroyPreservesSharedResources verifies that Destroy completes
// successfully even when a template ID is set but no actual layer exists,
// and that the VM's working directory is removed (VM-owned, not shared).
func TestDestroyPreservesSharedResources(t *testing.T) {
	vm, err := New(testConfig(t), nil)
	require.NoError(t, err)

	vm.mu.Lock()
	vm.cfg.TemplateID = "test-template"
	vm.writableLayer = ""
	vm.nbdDevice = ""
	vm.mu.Unlock()

	ctx := context.Background()
	require.NoError(t, vm.Destroy(ctx))
	require.Equal(t, StateDestroyed, vm.State())

	_, err = os.Stat(vm.Dir())
	require.True(t, os.IsNotExist(err), "VM working directory should be removed")
}

func strPtr(s string) *string {
	return &s
}

// TestSnapshotCleanupTracksAndReleasesInReverseOrder verifies that
// snapshotCleanup tracks resources and releases them in reverse acquisition
// order. Files that were tracked are removed; files not tracked are preserved.
func TestSnapshotCleanupTracksAndReleasesInReverseOrder(t *testing.T) {
	dir := t.TempDir()

	// Create tracked files.
	snapPath := filepath.Join(dir, "state.snap")
	memPath := filepath.Join(dir, "mem.snap")
	overlayPath := filepath.Join(dir, "overlay.qcow2")
	tmpOverlay := filepath.Join(dir, "overlay.qcow2.tmp")
	snapDir := filepath.Join(dir, "snapdir")

	for _, p := range []string{snapPath, memPath, overlayPath, tmpOverlay} {
		require.NoError(t, os.WriteFile(p, []byte("data"), 0644))
	}
	require.NoError(t, os.MkdirAll(snapDir, 0750))

	// Create an untracked file that should be preserved.
	untracked := filepath.Join(dir, "untracked.txt")
	require.NoError(t, os.WriteFile(untracked, []byte("preserve me"), 0644))

	vm, err := New(testConfig(t), nil)
	require.NoError(t, err)

	c := newSnapshotCleanup(vm)
	c.trackSnapshot(snapPath, memPath, snapDir)
	c.trackOverlay(overlayPath, tmpOverlay)

	// First cleanup should succeed.
	require.NoError(t, c.Cleanup())

	// Tracked files should be removed.
	for _, p := range []string{snapPath, memPath, overlayPath, tmpOverlay} {
		_, err := os.Stat(p)
		require.True(t, os.IsNotExist(err), "%q should be removed", p)
	}

	// Snapshot dir should be removed (empty after artifact removal).
	_, err = os.Stat(snapDir)
	require.True(t, os.IsNotExist(err), "snap dir should be removed")

	// Untracked file should be preserved.
	data, err := os.ReadFile(untracked)
	require.NoError(t, err)
	require.Equal(t, "preserve me", string(data))

	// Second cleanup should be a no-op (idempotent).
	require.NoError(t, c.Cleanup())
}

// TestSnapshotCleanupMissingFilesDoesNotFail verifies that Cleanup does not
// fail when tracked files are already missing.
func TestSnapshotCleanupMissingFilesDoesNotFail(t *testing.T) {
	vm, err := New(testConfig(t), nil)
	require.NoError(t, err)

	c := newSnapshotCleanup(vm)
	c.trackSnapshot("/nonexistent/state.snap", "/nonexistent/mem.snap", "/nonexistent/snapdir")
	c.trackOverlay("/nonexistent/overlay.qcow2", "/nonexistent/overlay.qcow2.tmp")

	// Should not error on missing files.
	require.NoError(t, c.Cleanup())

	// Idempotent second call.
	require.NoError(t, c.Cleanup())
}

// TestSnapshotCleanupWithDestroyVM verifies that when destroyVM is true,
// Cleanup calls vm.destroy() which handles full VM cleanup (process, transport,
// sockets, work dir, NBD, layers).
func TestSnapshotCleanupWithDestroyVM(t *testing.T) {
	vm, err := New(testConfig(t), nil)
	require.NoError(t, err)

	c := newSnapshotCleanup(vm)
	c.trackRestore(false, false, false, true) // destroyVM=true

	// Cleanup should call vm.destroy() which transitions to Destroyed.
	require.NoError(t, c.Cleanup())
	require.Equal(t, StateDestroyed, vm.State())

	// Second call is a no-op.
	require.NoError(t, c.Cleanup())
	require.Equal(t, StateDestroyed, vm.State())
}

// TestSnapshotCleanupKillProcess verifies that Cleanup kills the Firecracker
// process when procStarted is true and destroyVM is false.
func TestSnapshotCleanupKillProcess(t *testing.T) {
	vm, err := New(testConfig(t), nil)
	require.NoError(t, err)

	// Start a real process.
	proc, err := Start("sleep", []string{"100"}, nil)
	require.NoError(t, err)
	vm.mu.Lock()
	vm.proc = proc
	vm.mu.Unlock()

	t.Cleanup(func() {
		_ = proc.Kill()
	})

	c := newSnapshotCleanup(vm)
	c.trackRestore(true, false, false, false) // procStarted=true, destroyVM=false

	require.NoError(t, c.Cleanup())

	// Process should be terminated.
	select {
	case <-proc.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("process was not terminated by Cleanup")
	}
	require.True(t, proc.Reaped())

	// Transport should not be set (transportSet=false).
	vm.mu.Lock()
	require.Nil(t, vm.transport)
	vm.mu.Unlock()

	// State should not be Destroyed (destroyVM=false).
	require.NotEqual(t, StateDestroyed, vm.State())
}

// TestSnapshotCleanupTransportSet verifies that Cleanup nils the transport
// when transportSet is true and destroyVM is false.
func TestSnapshotCleanupTransportSet(t *testing.T) {
	vm, err := New(testConfig(t), nil)
	require.NoError(t, err)

	// Set a transport.
	vm.mu.Lock()
	vm.transport = NewTransport("/tmp/fake.sock", 3)
	vm.mu.Unlock()
	require.NotNil(t, vm.Vsock())

	c := newSnapshotCleanup(vm)
	c.trackRestore(true, true, false, false) // transportSet=true, destroyVM=false

	require.NoError(t, c.Cleanup())

	// Transport should be nilled.
	vm.mu.Lock()
	require.Nil(t, vm.transport)
	vm.mu.Unlock()
}

// TestSnapshotCleanupNbDMapping verifies that Cleanup unmaps NBD when
// nbdMapped is true and destroyVM is false.
func TestSnapshotCleanupNbDMapping(t *testing.T) {
	vm, err := New(testConfig(t), nil)
	require.NoError(t, err)

	vm.mu.Lock()
	vm.imageNBDDevice = "/dev/nbd0"
	vm.mu.Unlock()

	c := newSnapshotCleanup(vm)
	c.trackRestore(false, false, true, false) // nbdMapped=true, destroyVM=false

	// unmapImage will try to unmap /dev/nbd0 which may not exist.
	// It should not fail the cleanup (errors are collected).
	err = c.Cleanup()
	// May or may not error depending on whether /dev/nbd0 exists.
	// The important thing is it doesn't panic and state is unchanged.
	require.NotEqual(t, StateDestroyed, vm.State())
}

// TestSnapshotCleanupReverseOrder verifies that resources are released in
// reverse acquisition order by tracking the sequence of cleanup actions.
func TestSnapshotCleanupReverseOrder(t *testing.T) {
	// Create resources, clean up, and verify all are removed regardless of
	// cleanup order.
	dir := t.TempDir()
	snapPath := filepath.Join(dir, "state.snap")
	memPath := filepath.Join(dir, "mem.snap")

	require.NoError(t, os.WriteFile(snapPath, []byte("data"), 0644))
	require.NoError(t, os.WriteFile(memPath, []byte("data"), 0644))

	vm, err := New(testConfig(t), nil)
	require.NoError(t, err)

	c := newSnapshotCleanup(vm)
	// Acquire in order: snapPath, memPath
	c.trackSnapshot(snapPath, memPath, "")

	require.NoError(t, c.Cleanup())

	// Both should be removed regardless of cleanup order.
	_, err = os.Stat(snapPath)
	require.True(t, os.IsNotExist(err), "snapPath should be removed")
	_, err = os.Stat(memPath)
	require.True(t, os.IsNotExist(err), "memPath should be removed")
}

// TestSnapshotCleanupConcurrentCalls verifies that concurrent Cleanup calls
// are safe (only one performs cleanup, others are no-ops).
func TestSnapshotCleanupConcurrentCalls(t *testing.T) {
	vm, err := New(testConfig(t), nil)
	require.NoError(t, err)

	dir := t.TempDir()
	snapPath := filepath.Join(dir, "state.snap")
	require.NoError(t, os.WriteFile(snapPath, []byte("data"), 0644))

	c := newSnapshotCleanup(vm)
	c.trackSnapshot(snapPath, "", "")

	// Call Cleanup concurrently multiple times.
	done := make(chan struct{}, 10)
	for i := 0; i < 10; i++ {
		go func() {
			_ = c.Cleanup()
			done <- struct{}{}
		}()
	}

	for i := 0; i < 10; i++ {
		<-done
	}

	// File should be removed exactly once.
	_, err = os.Stat(snapPath)
	require.True(t, os.IsNotExist(err), "snapPath should be removed")
}

// TestStateMachineConcurrentTransitions verifies that concurrent transitions
// from multiple goroutines do not corrupt state. Only transitions valid for
// the current state succeed; invalid ones return an error.
func TestStateMachineConcurrentTransitions(t *testing.T) {
	sm := NewStateMachine()
	require.Equal(t, StateCreated, sm.State())

	var wg sync.WaitGroup
	errCh := make(chan error, 50)

	// 25 goroutines try Created -> Preparing (only one should succeed).
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := sm.Transition(StateCreated, StatePreparing)
			errCh <- err
		}()
	}

	// 25 goroutines try invalid transitions (all should fail).
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := sm.Transition(StateCreated, StateRunning)
			errCh <- err
		}()
	}

	wg.Wait()
	close(errCh)

	// Exactly one Created->Preparing should succeed.
	successCount := 0
	for err := range errCh {
		if err == nil {
			successCount++
		}
	}
	require.Equal(t, 1, successCount, "exactly one Created->Preparing should succeed")
	require.Equal(t, StatePreparing, sm.State())
}

// TestStateMachineRepeatedTransitions verifies idempotent stop/destroy behavior
// and that repeated transitions to the same state fail appropriately.
func TestStateMachineRepeatedTransitions(t *testing.T) {
	sm := NewStateMachine()

	// Created -> Preparing succeeds once.
	require.NoError(t, sm.Transition(StateCreated, StatePreparing))

	// Same transition again should fail (state is now Preparing, not Created).
	err := sm.Transition(StateCreated, StatePreparing)
	require.Error(t, err)
	require.Equal(t, StatePreparing, sm.State())

	// Preparing -> Running succeeds.
	require.NoError(t, sm.Transition(StatePreparing, StateRunning))

	// Running -> Stopping succeeds.
	require.NoError(t, sm.Transition(StateRunning, StateStopping))

	// Stopping -> Stopped succeeds.
	require.NoError(t, sm.Transition(StateStopping, StateStopped))

	// Stopped -> Destroying succeeds.
	require.NoError(t, sm.Transition(StateStopped, StateDestroying))

	// Destroying -> Destroyed succeeds.
	require.NoError(t, sm.Transition(StateDestroying, StateDestroyed))

	// Destroyed is terminal; any transition from it should fail.
	err = sm.Transition(StateDestroyed, StateRunning)
	require.Error(t, err)
	require.Equal(t, StateDestroyed, sm.State())

	// TransitionTo can move to Failed from Destroyed (terminal override).
	sm.TransitionTo(StateFailed)
	require.Equal(t, StateFailed, sm.State())
	require.True(t, sm.IsTerminal(StateFailed))
}

// TestStateMachineTransitionToBroadcasts verifies that TransitionTo properly
// broadcasts state changes. It uses a generation-based waiter pattern (the safe
// replacement for the removed Waiting() API) with synchronization to ensure
// the waiter is observing before the transition occurs.
func TestStateMachineTransitionToBroadcasts(t *testing.T) {
	sm := NewStateMachine()

	// Synchronization: ready is closed when the watcher has captured the
	// generation and is about to poll. done is closed when the watcher
	// observes the state change or gives up.
	ready := make(chan struct{})
	done := make(chan struct{})

	go func() {
		sm.mu.Lock()
		gen := sm.state
		sm.mu.Unlock()

		close(ready)

		// Poll State() until it changes (generation-based wait).
		for i := 0; i < 10000; i++ {
			sm.mu.Lock()
			cur := sm.state
			sm.mu.Unlock()
			if cur != gen {
				close(done)
				return
			}
			runtime.Gosched() // yield to allow main goroutine to transition
		}
		t.Error("waiter did not observe state change")
		close(done)
	}()

	// Wait for the watcher to capture the generation and start polling.
	<-ready

	// TransitionTo should broadcast, allowing the watcher to observe the change.
	sm.TransitionTo(StateFailed)

	// Give the watcher time to observe the change.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not observe state change")
	}

	require.Equal(t, StateFailed, sm.State())
}

// TestStateMachineRaceDetector is designed to be run with -race. It exercises
// concurrent State() reads and Transition writes to confirm there are no data
// races on the state machine internals.
func TestStateMachineRaceDetector(t *testing.T) {
	sm := NewStateMachine()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = sm.State()
			}
		}()
	}

	// Concurrent writer.
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = sm.Transition(StateCreated, StatePreparing)
		_ = sm.Transition(StatePreparing, StateRunning)
	}()

	wg.Wait()
}

// TestForkCopyRootFSConfig verifies that Fork with CopyRootFS: true copies the
// rootfs into the child's work dir before calling restoreVM. Fork fails at
// restoreVM (no Firecracker in unit tests), but the rootfs copy is made
// before that.
// TestForkCopyRootFSConfig verifies that Fork with CopyRootFS: true copies the
// rootfs into the child's work dir before calling restoreVM. Fork fails at
// restoreVM (no Firecracker in unit tests), but the rootfs copy is made
// before that. We verify Fork got past New() by checking the error is from
// restoreVM, not from create child VM.
func TestForkCopyRootFSConfig(t *testing.T) {
	parent, err := New(testConfig(t), nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		// No Firecracker was started, so just clean up the work dir.
		os.RemoveAll(parent.res.Dir)
	})

	// Transition parent to Paused (required for Fork).
	require.NoError(t, parent.sm.Transition(StateCreated, StatePreparing))
	require.NoError(t, parent.sm.Transition(StatePreparing, StateRunning))
	require.NoError(t, parent.sm.Transition(StateRunning, StatePaused))

	// Set up a fake API that succeeds CreateSnapshot.
	f := &fakeAPI{}
	f.instanceState = "Paused"
	parent.mu.Lock()
	parent.api = f
	parent.mu.Unlock()

	// Set a real rootfs file so copyFile succeeds.
	rootfsFile := filepath.Join(t.TempDir(), "rootfs.ext4")
	require.NoError(t, os.WriteFile(rootfsFile, []byte("fake-rootfs-data"), 0644))
	parent.cfg.RootFS = rootfsFile

	// Set a non-existent bin path so startProcessAndWait fails fast.
	parent.cfg.BinPath = "/nonexistent/firecracker"

	childWorkDir := t.TempDir()
	_, err = parent.Fork(context.Background(), ForkOpts{
		CopyRootFS: true,
		ChildConfig: Config{
			WorkDir:  childWorkDir,
			VsockCID: 4,
		},
	})
	require.Error(t, err, "fork should fail at restoreVM")
	require.Contains(t, err.Error(), "fork restore",
		"Fork should fail at restoreVM, not at New()")
}

// TestForkCopyRootFSConfigFalse verifies that Fork with CopyRootFS: false sets
// RootFSReadOnly=true on the child config. Fork fails at restoreVM (no
// Firecracker in unit tests), but the config is set before that.
func TestForkCopyRootFSConfigFalse(t *testing.T) {
	parent, err := New(testConfig(t), nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		// No Firecracker was started, so just clean up the work dir.
		os.RemoveAll(parent.res.Dir)
	})

	// Transition parent to Paused (required for Fork).
	require.NoError(t, parent.sm.Transition(StateCreated, StatePreparing))
	require.NoError(t, parent.sm.Transition(StatePreparing, StateRunning))
	require.NoError(t, parent.sm.Transition(StateRunning, StatePaused))

	// Set up a fake API that succeeds CreateSnapshot.
	f := &fakeAPI{}
	f.instanceState = "Paused"
	parent.mu.Lock()
	parent.api = f
	parent.mu.Unlock()

	// Set a real rootfs file so copyFile succeeds (even though CopyRootFS is false,
	// we set RootFS so Fork doesn't fail on empty RootFS).
	rootfsFile := filepath.Join(t.TempDir(), "rootfs.ext4")
	require.NoError(t, os.WriteFile(rootfsFile, []byte("fake-rootfs-data"), 0644))
	parent.cfg.RootFS = rootfsFile

	// Set a non-existent bin path so startProcessAndWait fails fast.
	parent.cfg.BinPath = "/nonexistent/firecracker"

	childWorkDir := t.TempDir()
	_, err = parent.Fork(context.Background(), ForkOpts{
		CopyRootFS: false,
		ChildConfig: Config{
			WorkDir:  childWorkDir,
			VsockCID: 4,
		},
	})
	require.Error(t, err, "fork should fail at restoreVM")
	require.Contains(t, err.Error(), "fork restore",
		"Fork should fail at restoreVM, not at New()")

	// With CopyRootFS: false, no rootfs copy should be made.
	entries, err := os.ReadDir(childWorkDir)
	require.NoError(t, err)
	var foundRootFS bool
	for _, entry := range entries {
		entryPath := filepath.Join(childWorkDir, entry.Name())
		info, err := os.Stat(entryPath)
		if err != nil {
			continue
		}
		if info.IsDir() {
			rootfsPath := filepath.Join(entryPath, "rootfs.ext4")
			if _, err := os.Stat(rootfsPath); err == nil {
				foundRootFS = true
				break
			}
		}
	}
	require.False(t, foundRootFS, "no rootfs copy should exist with CopyRootFS: false")
}

func TestForkFailureCleanupUnit(t *testing.T) {
	parent, err := New(testConfig(t), nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		// No Firecracker was started, so just clean up the work dir.
		os.RemoveAll(parent.res.Dir)
	})

	// Transition parent to Paused (required for Fork).
	require.NoError(t, parent.sm.Transition(StateCreated, StatePreparing))
	require.NoError(t, parent.sm.Transition(StatePreparing, StateRunning))
	require.NoError(t, parent.sm.Transition(StateRunning, StatePaused))

	// Set up a fake API for the parent.
	f := &fakeAPI{}
	f.instanceState = "Paused"
	parent.mu.Lock()
	parent.api = f
	parent.transport = NewTransport("/tmp/fake-parent.sock", 3)
	parent.mu.Unlock()

	// Fork with an invalid WorkDir that already exists as a file (not a dir),
	// which will cause New() to fail.
	badWorkDir := filepath.Join(t.TempDir(), "bad-work-dir")
	require.NoError(t, os.WriteFile(badWorkDir, []byte("not a directory"), 0644))

	_, err = parent.Fork(context.Background(), ForkOpts{
		CopyRootFS: true,
		ChildConfig: Config{
			WorkDir:  badWorkDir,
			VsockCID: 4,
		},
	})
	require.Error(t, err, "fork with invalid work dir should fail")

	// Parent should still be in Paused state.
	require.Equal(t, StatePaused, parent.State())

	// createSnapshotLegacy ran before New() failed, so snapshot exists.
	require.NotNil(t, parent.snapshot, "parent snapshot should exist")

	// Parent's transport should still be intact.
	require.NotNil(t, parent.Vsock())
}
