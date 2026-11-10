package vmm

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeConn is an in-memory io.ReadWriteCloser for testing runExec. Writes are
// captured; reads are served from a pre-programmed response queue, then EOF.
type fakeConn struct {
	mu        sync.Mutex
	writes    []string
	responses []string // responses to serve in order
	index     int
	closed    bool
}

func (f *fakeConn) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, string(p))
	return len(p), nil
}

func (f *fakeConn) Read(p []byte) (int, error) {
	f.mu.Lock()
	if f.index >= len(f.responses) {
		f.closed = true
		f.mu.Unlock()
		return 0, io.EOF
	}
	data := []byte(f.responses[f.index] + "\n")
	f.index++
	f.mu.Unlock()
	copy(p, data)
	return len(data), nil
}

func (f *fakeConn) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeConn) lastWrite() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.writes) == 0 {
		return ""
	}
	return f.writes[len(f.writes)-1]
}

func DefaultConfigWithArtifacts() Config {
	c := DefaultConfig()
	c.KernelImage = "/home/omkar/.local/firecracker/vmlinux"
	c.RootFS = "/home/omkar/.local/firecracker/alpine/alpine-rootfs.ext4"
	return c
}

func TestRunExecStreamsOutputAndReturnsExitCode(t *testing.T) {
	conn := &fakeConn{
		responses: []string{
			`{"kind":"stdout","data":"hello world\n"}`,
			`{"kind":"stderr","data":"warning\n"}`,
			`{"kind":"exit","code":42}`,
		},
	}

	stdout := &strings.Builder{}
	stderr := &strings.Builder{}
	code, err := runExec(context.Background(), conn, Workload{
		Cmd: "/bin/sh", Args: []string{"-c", "echo hello world; echo warning >&2; exit 42"},
	}, stdout, stderr)
	require.NoError(t, err)
	require.Equal(t, 42, code)
	require.Equal(t, "hello world\n", stdout.String())
	require.Equal(t, "warning\n", stderr.String())

	req := conn.lastWrite()
	require.Contains(t, req, `"kind":"exec"`)
	require.Contains(t, req, `"/bin/sh"`)
}

func TestRunExecPropagatesGuestError(t *testing.T) {
	conn := &fakeConn{
		responses: []string{`{"kind":"error","message":"command not found"}`},
	}

	code, err := runExec(context.Background(), conn, Workload{Cmd: "nope"}, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "command not found")
	require.Equal(t, -1, code)
}

func TestRunExecContextCancellation(t *testing.T) {
	conn := &fakeConn{
		responses: []string{`{"kind":"stdout","data":"never\n"}`},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := runExec(ctx, conn, Workload{Cmd: "sleep"}, nil, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}

func TestRunExecEOFBeforeExit(t *testing.T) {
	conn := &fakeConn{responses: []string{}}

	code, err := runExec(context.Background(), conn, Workload{Cmd: "true"}, nil, nil)
	require.NoError(t, err)
	require.Equal(t, -1, code)
}

func TestSandboxExecuteRejectsNonRunningVM(t *testing.T) {
	vm, err := New(DefaultConfigWithArtifacts(), nil)
	require.NoError(t, err)
	s := &Sandbox{vm: vm}

	code, err := s.Execute(context.Background(), Workload{Cmd: "true"}, nil, nil)
	require.Error(t, err)
	require.Equal(t, -1, code)
	require.Contains(t, err.Error(), "not running")
}

func TestSandboxCloseIdempotent(t *testing.T) {
	vm, err := New(DefaultConfigWithArtifacts(), nil)
	require.NoError(t, err)
	s := &Sandbox{vm: vm}

	require.NoError(t, s.Close(context.Background()))
	require.NoError(t, s.Close(context.Background()))
}

func TestNewSandboxAssignsUniqueCID(t *testing.T) {
	s1, err := NewSandbox(DefaultConfigWithArtifacts())
	require.NoError(t, err)
	s2, err := NewSandbox(DefaultConfigWithArtifacts())
	require.NoError(t, err)
	require.NotEqual(t, s1.cfg.VsockCID, s2.cfg.VsockCID)
	require.GreaterOrEqual(t, s1.cfg.VsockCID, uint32(3))
	require.GreaterOrEqual(t, s2.cfg.VsockCID, uint32(3))
}

func TestSandboxExecuteSendsCorrectRequest(t *testing.T) {
	conn := &fakeConn{
		responses: []string{`{"kind":"exit","code":0}`},
	}

	_, err := runExec(context.Background(), conn, Workload{
		Cmd:     "/bin/echo",
		Args:    []string{"hello"},
		Env:     []string{"FOO=bar"},
		Dir:     "/tmp",
		Timeout: 5 * time.Second,
	}, nil, nil)
	require.NoError(t, err)

	req := conn.lastWrite()
	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(req), &parsed))
	require.Equal(t, "exec", parsed["kind"])
	require.Equal(t, "/bin/echo", parsed["cmd"])
	require.Equal(t, []any{"hello"}, parsed["args"])
	require.Equal(t, []any{"FOO=bar"}, parsed["env"])
	require.Equal(t, "/tmp", parsed["cwd"])
	require.Equal(t, float64(5000), parsed["timeout_ms"])
}
