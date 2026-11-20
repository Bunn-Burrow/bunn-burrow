package vmm

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// Transport provides generic host-to-guest vsock connectivity for a single
// Firecracker VM. It is bound to one VM's vsock UDS socket and guest CID, so
// connections cannot cross VM boundaries: each VM has its own Transport.
//
// Firecracker listens on a Unix domain socket (uds_path). A host connection is
// established by dialing that socket and sending the handshake
//
//	CONNECT <guest-port>\n
//
// and reading the acknowledgement
//
//	OK <assigned-host-port>\n
//
// After the handshake, the connection is a regular net.Conn to the guest's
// vsock port.
//
// The guest must run a vsock listener bound to the target port for a
// connection to succeed.
type Transport struct {
	socketPath string
	cid        uint32
}

// NewTransport creates a vsock transport for the given VM. socketPath is the
// Firecracker vsock UDS path; cid is the guest vsock CID.
func NewTransport(socketPath string, cid uint32) *Transport {
	return &Transport{socketPath: socketPath, cid: cid}
}

// Dial connects to the guest at the given vsock port. It performs the
// Firecracker host-initiated connection handshake over the VM's vsock UDS
// socket. The context is honored for cancellation and deadlines. The returned
// net.Conn must be closed by the caller.
func (t *Transport) Dial(ctx context.Context, port uint32) (net.Conn, error) {
	if t.socketPath == "" {
		return nil, errors.New("vsock socket path is not configured")
	}
	if port == 0 {
		return nil, errors.New("vsock port must be non-zero")
	}

	// Dial deadline is bounded by the context.
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	d := net.Dialer{}
	conn, err := d.DialContext(dialCtx, "unix", t.socketPath)
	if err != nil {
		return nil, fmt.Errorf("vsock dial to %q: %w", t.socketPath, err)
	}

	// Perform the host-initiated connection handshake.
	if err := t.handshake(conn, port); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// handshake writes the CONNECT message and reads the OK acknowledgement.
func (t *Transport) handshake(conn net.Conn, port uint32) error {
	msg := fmt.Sprintf("CONNECT %d\n", port)

	if err := writeDeadline(conn, []byte(msg), 5*time.Second); err != nil {
		return fmt.Errorf("write connect message: %w", err)
	}

	line, err := readUntil(conn, '\n', 5*time.Second)
	if err != nil {
		return fmt.Errorf("read connect acknowledgement: %w", err)
	}
	if !strings.HasPrefix(line, "OK ") {
		return fmt.Errorf("expected \"OK <port>\", got %q", line)
	}
	return nil
}

func writeDeadline(conn net.Conn, b []byte, d time.Duration) error {
	conn.SetWriteDeadline(time.Now().Add(d))
	defer conn.SetWriteDeadline(time.Time{})
	written, err := conn.Write(b)
	if err != nil {
		return err
	}
	if written != len(b) {
		return errors.New("incomplete connect message write")
	}
	return nil
}

func readUntil(conn net.Conn, end byte, d time.Duration) (string, error) {
	conn.SetReadDeadline(time.Now().Add(d))
	defer conn.SetReadDeadline(time.Time{})
	line, err := bufio.NewReaderSize(conn, 64).ReadString(end)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\n"), nil
}

// SocketPath returns the Firecracker vsock UDS path this transport uses.
func (t *Transport) SocketPath() string { return t.socketPath }

// CID returns the guest vsock CID this transport targets.
func (t *Transport) CID() uint32 { return t.cid }

// SendRequest sends a JSON request to the guest agent at the given vsock port
// and returns the parsed JSON response. It uses length-prefixed framing
// (4-byte big-endian length + JSON payload) matching the guest agent's vsock
// server protocol. The context is honored for cancellation and deadlines.
// The returned net.Conn is closed by SendRequest before returning.
func (t *Transport) SendRequest(ctx context.Context, port uint32, req map[string]any) (map[string]any, error) {
	conn, err := t.Dial(ctx, port)
	if err != nil {
		return nil, fmt.Errorf("vsock dial: %w", err)
	}
	defer conn.Close()

	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))
	if err := writeFull(conn, lenBuf[:]); err != nil {
		return nil, fmt.Errorf("write request length: %w", err)
	}
	if err := writeFull(conn, payload); err != nil {
		return nil, fmt.Errorf("write request payload: %w", err)
	}

	var respLenBuf [4]byte
	if _, err := io.ReadFull(conn, respLenBuf[:]); err != nil {
		return nil, fmt.Errorf("read response length: %w", err)
	}
	frameLen := binary.BigEndian.Uint32(respLenBuf[:])
	if frameLen == 0 || frameLen > 1<<20 {
		return nil, fmt.Errorf("invalid response frame length: %d", frameLen)
	}
	respPayload := make([]byte, frameLen)
	if _, err := io.ReadFull(conn, respPayload); err != nil {
		return nil, fmt.Errorf("read response payload: %w", err)
	}

	var resp map[string]any
	if err := json.Unmarshal(respPayload, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	return resp, nil
}
