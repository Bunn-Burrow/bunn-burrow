package vmm

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeConn is an in-memory io.ReadWriteCloser for testing runExec. Writes are
// captured for request inspection; reads are served from a pre-programmed
// queue of response payloads, each framed with a 4-byte big-endian length
// prefix, matching the guest agent vsock protocol.
type fakeConn struct {
	mu        sync.Mutex
	writeBuf  []byte   // all bytes written
	responses []string // response payloads to serve, in order
	respBuf   []byte   // concatenated framed responses
	respOff   int      // read offset within respBuf
	closed    bool
}

func (f *fakeConn) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeBuf = append(f.writeBuf, p...)
	return len(p), nil
}

func (f *fakeConn) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.respOff >= len(f.respBuf) {
		if len(f.responses) == 0 {
			f.closed = true
			return 0, io.EOF
		}
		// Build the next frame.
		payload := []byte(f.responses[0])
		f.responses = f.responses[1:]
		var lenBuf [4]byte
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))
		f.respBuf = append(lenBuf[:], payload...)
		f.respOff = 0
	}
	n := copy(p, f.respBuf[f.respOff:])
	f.respOff += n
	return n, nil
}

func (f *fakeConn) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// lastRequest returns the last complete JSON request payload written to the
// connection, parsed from the length-prefixed framing.
func (f *fakeConn) lastRequest() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	buf := f.writeBuf
	offset := 0
	var lastPayload string
	for offset+4 <= len(buf) {
		frameLen := binary.BigEndian.Uint32(buf[offset : offset+4])
		offset += 4
		if offset+int(frameLen) > len(buf) {
			break
		}
		lastPayload = string(buf[offset : offset+int(frameLen)])
		offset += int(frameLen)
	}
	return lastPayload
}

// ServeFrame enqueues a response payload to be served on subsequent Read calls.
func (f *fakeConn) ServeFrame(payload string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses = append(f.responses, payload)
}

func DefaultConfigWithArtifacts() Config {
	c := DefaultConfig()
	c.KernelImage = "/home/omkar/.local/firecracker/vmlinux"
	c.RootFS = "/home/omkar/.local/firecracker/alpine/alpine-rootfs.ext4"
	return c
}

func TestRunExecStreamsOutputAndReturnsExitCode(t *testing.T) {
	conn := &fakeConn{}
	conn.ServeFrame(`{"kind":"execution_output","stream":"stdout","data":"hello world\n"}`)
	conn.ServeFrame(`{"kind":"execution_output","stream":"stderr","data":"warning\n"}`)
	conn.ServeFrame(`{"kind":"execution_finished","exit_code":42}`)

	stdout := &strings.Builder{}
	stderr := &strings.Builder{}
	code, err := runExec(context.Background(), conn, Workload{
		Cmd: "/bin/sh",
		Args: []string{"-c", "echo hello world; echo warning >&2; exit 42"},
	}, stdout, stderr, nil)
	require.NoError(t, err)
	require.Equal(t, 42, code)
	require.Equal(t, "hello world\n", stdout.String())
	require.Equal(t, "warning\n", stderr.String())

	req := conn.lastRequest()
	require.Contains(t, req, `"kind":"execute"`)
	require.Contains(t, req, `"/bin/sh"`)
	require.Contains(t, req, `"timeout_seconds":0`)
}

func TestRunExecPropagatesGuestError(t *testing.T) {
	conn := &fakeConn{}
	conn.ServeFrame(`{"kind":"error","message":"command not found"}`)

	code, err := runExec(context.Background(), conn, Workload{Cmd: "nope"}, nil, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "command not found")
	require.Equal(t, -1, code)
}

func TestRunExecContextCancellation(t *testing.T) {
	conn := &fakeConn{}
	conn.ServeFrame(`{"kind":"execution_output","stream":"stdout","data":"never\n"}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := runExec(ctx, conn, Workload{Cmd: "sleep"}, nil, nil, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}

func TestRunExecEOFBeforeExit(t *testing.T) {
	conn := &fakeConn{}

	code, err := runExec(context.Background(), conn, Workload{Cmd: "true"}, nil, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "closed before execution finished")
	require.Equal(t, -1, code)
}

func TestRunExecUnexpectedEOFBeforeExit(t *testing.T) {
	// A conn that returns io.ErrUnexpectedEOF mid-frame simulates a partial
	// read before execution_finished.
	conn := &prematureEOFConn{}

	code, err := runExec(context.Background(), conn, Workload{Cmd: "true"}, nil, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "closed before execution finished")
	require.Equal(t, -1, code)
}

func TestRunExecStdinSent(t *testing.T) {
	conn := &fakeConn{}
	conn.ServeFrame(`{"kind":"execution_finished","exit_code":0}`)

	stdin := strings.NewReader("input data\n")
	_, err := runExec(context.Background(), conn, Workload{
		Cmd:   "cat",
		Stdin: stdin,
	}, nil, nil, stdin)
	require.NoError(t, err)

	req := conn.lastRequest()
	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(req), &parsed))
	require.Equal(t, "execute", parsed["kind"])
	require.Equal(t, "input data\n", parsed["stdin"])
}

func TestRunExecOversizedStdinRejected(t *testing.T) {
	conn := &fakeConn{}
	conn.ServeFrame(`{"kind":"execution_finished","exit_code":0}`)

	// Stdin larger than MaxStdinSize.
	oversized := make([]byte, MaxStdinSize+1)
	for i := range oversized {
		oversized[i] = 'x'
	}
	stdin := bytes.NewReader(oversized)

	_, err := runExec(context.Background(), conn, Workload{
		Cmd:   "cat",
		Stdin: stdin,
	}, nil, nil, stdin)
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds maximum size")
}

func TestRunExecMaxStdinSizeAccepted(t *testing.T) {
	conn := &fakeConn{}
	conn.ServeFrame(`{"kind":"execution_finished","exit_code":0}`)

	// Stdin exactly at MaxStdinSize should be accepted.
	atLimit := make([]byte, MaxStdinSize)
	for i := range atLimit {
		atLimit[i] = 'x'
	}
	stdin := bytes.NewReader(atLimit)

	_, err := runExec(context.Background(), conn, Workload{
		Cmd:   "cat",
		Stdin: stdin,
	}, nil, nil, stdin)
	require.NoError(t, err)
}

// prematureEOFConn returns io.ErrUnexpectedEOF on the first Read, simulating
// a guest that closes the vsock connection before sending execution_finished.
type prematureEOFConn struct{}

func (prematureEOFConn) Write(p []byte) (int, error) { return len(p), nil }
func (prematureEOFConn) Read(p []byte) (int, error)  { return 0, io.ErrUnexpectedEOF }
func (prematureEOFConn) Close() error                { return nil }

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
	conn := &fakeConn{}
	conn.ServeFrame(`{"kind":"execution_finished","exit_code":0}`)

	_, err := runExec(context.Background(), conn, Workload{
		Cmd:     "/bin/echo",
		Args:    []string{"hello"},
		Env:     []string{"FOO=bar"},
		Dir:     "/tmp",
		Timeout: 5 * time.Second,
	}, nil, nil, nil)
	require.NoError(t, err)

	req := conn.lastRequest()
	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(req), &parsed))
	require.Equal(t, "execute", parsed["kind"])
	require.Equal(t, "/bin/echo", parsed["command"])
	require.Equal(t, []any{"hello"}, parsed["args"])
	require.Equal(t, []any{"FOO=bar"}, parsed["env"])
	require.Equal(t, "/tmp", parsed["working_dir"])
	require.Equal(t, float64(5), parsed["timeout_seconds"])
}
