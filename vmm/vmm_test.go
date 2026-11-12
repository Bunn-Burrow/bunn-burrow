package vmm

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/stretchr/testify/require"
)

// fakeAPI implements API for tests.
type fakeAPI struct {
	mu                  sync.Mutex
	calls               []string
	instanceState       string
	machineCfg          *models.MachineConfiguration
	lastVsock           *models.Vsock
	lastAction          string
	putMachineCfgErr    error
	putBootSourceErr    error
	putGuestDriveErr    error
	putGuestVsockErr    error
	putGuestNetIfaceErr error
	createSyncActionErr error
	patchVMErr          error
	loadSnapshotErr     error
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
	return nil
}
func (f *fakeAPI) LoadSnapshot(ctx context.Context, p *models.SnapshotLoadParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "LoadSnapshot")
	return f.loadSnapshotErr
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
	vm, err := New(testConfig(&testing.T{}), nil)
	require.NoError(t, err)
	require.NotEmpty(t, vm.ID())
	require.Equal(t, StateCreated, vm.State())
	require.Equal(t, vm.ID(), filepath.Base(vm.Dir()))
	require.NotEmpty(t, vm.Dir())
	require.NotEmpty(t, vm.res.APISock)
	require.NotEmpty(t, vm.res.VsockSock)
	require.NotEqual(t, vm.res.APISock, vm.res.VsockSock)
}

func TestVMNewWithID(t *testing.T) {
	c := testConfig(&testing.T{})
	c.ID = "my-vm"
	vm, err := New(c, nil)
	require.NoError(t, err)
	require.Equal(t, "my-vm", vm.ID())
}

func TestPauseResumeWithFakeAPI(t *testing.T) {
	vm, err := New(testConfig(&testing.T{}), nil)
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
	vm, err := New(testConfig(&testing.T{}), nil)
	require.NoError(t, err)
	ctx := context.Background()
	require.Error(t, vm.Pause(ctx)) // Created -> not Running
	require.Equal(t, StateCreated, vm.State())
}

func TestStopWithNoProcessTimesOut(t *testing.T) {
	c := testConfig(&testing.T{})
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
	vm, err := New(testConfig(&testing.T{}), nil)
	require.NoError(t, err)
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
