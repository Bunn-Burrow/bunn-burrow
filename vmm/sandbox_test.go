package vmm

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
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
	if bin, kernel, rootfs, ok := integrationAssets(); ok {
		c.BinPath = bin
		c.KernelImage = kernel
		c.RootFS = rootfs
	} else {
		c.KernelImage = "/dev/null"
		c.RootFS = "/dev/null"
	}
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
		Cmd:  "/bin/sh",
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
		Env:     []string{"FOO=bar=baz"},
		Dir:     "/tmp",
		Timeout: 5 * time.Second,
	}, nil, nil, nil)
	require.NoError(t, err)

	req := conn.lastRequest()
	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(req), &parsed))
	require.Equal(t, "execute", parsed["kind"])
	require.Equal(t, "exec", parsed["execution_id"])
	require.Equal(t, "/bin/echo", parsed["command"])
	require.Equal(t, []any{"hello"}, parsed["args"])
	require.Equal(t, map[string]any{"FOO": "bar=baz"}, parsed["env"])
	require.Equal(t, "/tmp", parsed["working_dir"])
	require.Equal(t, float64(5), parsed["timeout_seconds"])
}

func TestSandboxExecuteSendsCustomExecutionID(t *testing.T) {
	conn := &fakeConn{}
	conn.ServeFrame(`{"kind":"execution_finished","exit_code":0}`)

	_, err := runExec(context.Background(), conn, Workload{
		ID:      "agent-exec-1234",
		Cmd:     "/bin/echo",
		Args:    []string{"hello"},
		Timeout: 5 * time.Second,
	}, nil, nil, nil)
	require.NoError(t, err)

	req := conn.lastRequest()
	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(req), &parsed))
	require.Equal(t, "execute", parsed["kind"])
	require.Equal(t, "agent-exec-1234", parsed["execution_id"])
}

func TestValidateProject(t *testing.T) {
	t.Run("valid project", func(t *testing.T) {
		p := &Project{
			ID: "test-proj",
			Files: map[string][]byte{
				"main.go": []byte("package main\n"),
				"go.mod":  []byte("module test\n"),
			},
		}
		require.NoError(t, validateProject(p))
	})

	t.Run("nil project rejected", func(t *testing.T) {
		require.Error(t, validateProject(nil))
	})

	t.Run("empty id rejected", func(t *testing.T) {
		p := &Project{Files: map[string][]byte{"main.go": []byte("x")}}
		require.Error(t, validateProject(p))
	})

	t.Run("no files rejected", func(t *testing.T) {
		p := &Project{ID: "test-proj"}
		require.Error(t, validateProject(p))
	})

	t.Run("too many files rejected", func(t *testing.T) {
		files := make(map[string][]byte, MaxProjectFiles+1)
		for i := 0; i <= MaxProjectFiles; i++ {
			files[string(rune('a'+i))+".txt"] = []byte("x")
		}
		p := &Project{ID: "test-proj", Files: files}
		require.Error(t, validateProject(p))
	})

	t.Run("absolute path rejected", func(t *testing.T) {
		p := &Project{
			ID:    "test-proj",
			Files: map[string][]byte{"/etc/passwd": []byte("x")},
		}
		require.Error(t, validateProject(p))
	})

	t.Run("path traversal rejected", func(t *testing.T) {
		p := &Project{
			ID:    "test-proj",
			Files: map[string][]byte{"../secret.txt": []byte("x")},
		}
		require.Error(t, validateProject(p))
	})

	t.Run("oversized project rejected", func(t *testing.T) {
		p := &Project{
			ID: "test-proj",
			Files: map[string][]byte{
				"big.txt": bytes.Repeat([]byte("x"), MaxProjectSize+1),
			},
		}
		require.Error(t, validateProject(p))
	})

	t.Run("oversized individual file rejected", func(t *testing.T) {
		p := &Project{
			ID: "test-proj",
			Files: map[string][]byte{
				"big.txt": bytes.Repeat([]byte("x"), MaxStdinSize+1),
			},
		}
		require.Error(t, validateProject(p))
	})
}

func TestValidateProjectPathHost(t *testing.T) {
	t.Run("valid paths", func(t *testing.T) {
		require.NoError(t, validateProjectPath("main.go"))
		require.NoError(t, validateProjectPath("dir/subdir/file.go"))
		require.NoError(t, validateProjectPath("config/app.yaml"))
	})

	t.Run("empty path rejected", func(t *testing.T) {
		require.Error(t, validateProjectPath(""))
	})

	t.Run("absolute path rejected", func(t *testing.T) {
		require.Error(t, validateProjectPath("/etc/passwd"))
	})

	t.Run("dot-dot traversal rejected", func(t *testing.T) {
		require.Error(t, validateProjectPath("../etc/passwd"))
		require.Error(t, validateProjectPath("dir/../../etc/passwd"))
		require.Error(t, validateProjectPath(".."))
	})
}

func TestProjectToTar(t *testing.T) {
	p := &Project{
		ID: "test-proj",
		Files: map[string][]byte{
			"main.go":              []byte("package main\n"),
			"go.mod":               []byte("module test\n"),
			"nested/dir/server.go": []byte("package server\n"),
		},
		DirPerm:  0o750,
		FilePerm: 0o640,
	}

	tarBuf, err := projectToTar(p)
	require.NoError(t, err)
	require.NotEmpty(t, tarBuf)

	// Verify the tar can be read back.
	tr := tar.NewReader(bytes.NewReader(tarBuf))
	found := make(map[string]bool)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		found[hdr.Name] = true

		// Verify permissions.
		if hdr.Typeflag == tar.TypeDir {
			require.Equal(t, int64(0o750), hdr.Mode)
		} else if hdr.Typeflag == tar.TypeReg {
			require.Equal(t, int64(0o640), hdr.Mode)
		}
	}

	require.True(t, found["main.go"])
	require.True(t, found["go.mod"])
	require.True(t, found["nested/dir/server.go"])
	require.True(t, found["nested"])
	require.True(t, found["nested/dir"])
}

func TestProjectToTarEmptyFile(t *testing.T) {
	p := &Project{
		ID: "test-proj",
		Files: map[string][]byte{
			"empty.txt": []byte{},
		},
	}

	tarBuf, err := projectToTar(p)
	require.NoError(t, err)

	tr := tar.NewReader(bytes.NewReader(tarBuf))
	hdr, err := tr.Next()
	require.NoError(t, err)
	require.Equal(t, "empty.txt", hdr.Name)
	require.Equal(t, int64(0), hdr.Size)
}

func TestSessionStartRequest(t *testing.T) {
	conn := &fakeConn{}
	conn.ServeFrame(`{"kind":"session_started","session_id":"sess-1","pid":123}`)

	ctx := context.Background()
	sess, err := StartSessionTest(ctx, conn, SessionConfig{
		Cmd:     "/bin/sh",
		Args:    []string{"-c", "echo hi"},
		Env:     []string{"FOO=bar"},
		Dir:     "/tmp",
		Rows:    24,
		Cols:    80,
		Timeout: 30 * time.Second,
	})
	require.NoError(t, err)
	require.NotNil(t, sess)

	req := conn.lastRequest()
	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(req), &parsed))
	require.Equal(t, "session_start", parsed["kind"])
	require.Equal(t, "/bin/sh", parsed["command"])
	require.Equal(t, []any{"-c", "echo hi"}, parsed["args"])
	require.Equal(t, map[string]any{"FOO": "bar"}, parsed["env"])
	require.Equal(t, "/tmp", parsed["working_dir"])
	require.Equal(t, float64(24), parsed["rows"])
	require.Equal(t, float64(80), parsed["cols"])
	require.Equal(t, float64(30), parsed["timeout_seconds"])
	require.Equal(t, sess.sessionID, parsed["session_id"])

	_ = sess.Close()
}

func TestSessionSendInput(t *testing.T) {
	conn := &fakeConn{}
	conn.ServeFrame(`{"kind":"session_started","session_id":"sess-1","pid":123}`)

	ctx := context.Background()
	sess, err := StartSessionTest(ctx, conn, SessionConfig{
		Cmd: "cat",
	})
	require.NoError(t, err)

	data := []byte("hello\n")
	require.NoError(t, sess.SendInput(data))

	req := conn.lastRequest()
	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(req), &parsed))
	require.Equal(t, "session_input", parsed["kind"])
	require.Equal(t, sess.sessionID, parsed["session_id"])
	require.Equal(t, "hello\n", parsed["data"])

	_ = sess.Close()
}

func TestSessionResize(t *testing.T) {
	conn := &fakeConn{}
	conn.ServeFrame(`{"kind":"session_started","session_id":"sess-1","pid":123}`)

	ctx := context.Background()
	sess, err := StartSessionTest(ctx, conn, SessionConfig{Cmd: "cat"})
	require.NoError(t, err)

	require.NoError(t, sess.Resize(40, 120))

	req := conn.lastRequest()
	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(req), &parsed))
	require.Equal(t, "session_resize", parsed["kind"])
	require.Equal(t, sess.sessionID, parsed["session_id"])
	require.Equal(t, float64(40), parsed["rows"])
	require.Equal(t, float64(120), parsed["cols"])

	_ = sess.Close()
}

func TestNewSessionIDUnique(t *testing.T) {
	id1 := newSessionID()
	id2 := newSessionID()
	require.NotEqual(t, id1, id2)
	require.True(t, strings.HasPrefix(id1, "sess-"))
	require.True(t, strings.HasPrefix(id2, "sess-"))
}

// StartSessionTest is a test helper that simulates Sandbox.StartSession using a
// fake connection. It sends session_start, reads session_started, and returns
// a Session for testing the session API without a real VM.
func StartSessionTest(ctx context.Context, conn *fakeConn, cfg SessionConfig) (*Session, error) {
	sessionID := newSessionID()
	env, err := envSliceToMap(cfg.Env)
	if err != nil {
		return nil, err
	}

	req := map[string]any{
		"kind":            "session_start",
		"session_id":      sessionID,
		"command":         cfg.Cmd,
		"args":            cfg.Args,
		"env":             env,
		"working_dir":     cfg.Dir,
		"rows":            cfg.Rows,
		"cols":            cfg.Cols,
		"timeout_seconds": cfg.Timeout.Seconds(),
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))
	if err := writeFull(conn, lenBuf[:]); err != nil {
		return nil, err
	}
	if err := writeFull(conn, payload); err != nil {
		return nil, err
	}

	msgCh := make(chan decodeResult, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var lenBuf [4]byte
			if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
				msgCh <- decodeResult{err: err}
				return
			}
			frameLen := binary.BigEndian.Uint32(lenBuf[:])
			if frameLen == 0 || frameLen > 1<<20 {
				msgCh <- decodeResult{err: fmt.Errorf("invalid frame length: %d", frameLen)}
				return
			}
			payload := make([]byte, frameLen)
			if _, err := io.ReadFull(conn, payload); err != nil {
				msgCh <- decodeResult{err: err}
				return
			}
			var msg map[string]any
			if err := json.Unmarshal(payload, &msg); err != nil {
				msgCh <- decodeResult{err: err}
				return
			}
			msgCh <- decodeResult{msg: msg}
		}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res, ok := <-msgCh:
		if !ok {
			return nil, errors.New("connection closed")
		}
		if res.err != nil {
			return nil, res.err
		}
		if kind, _ := res.msg["kind"].(string); kind != "session_started" {
			return nil, fmt.Errorf("unexpected response: %v", res.msg)
		}
		sess := &Session{
			conn:      conn,
			sessionID: sessionID,
			done:      make(chan struct{}),
			outputCh:  make(chan []byte, 128),
			errCh:     make(chan error, 1),
		}
		go sess.readLoop(msgCh)
		return sess, nil
	}
}
