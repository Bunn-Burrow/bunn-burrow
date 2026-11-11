package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/omkar/sandbox/vmm"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("vmm", flag.ExitOnError)
	kernel := fs.String("kernel", os.Getenv("FIRECRACKER_KERNEL"), "kernel image path")
	rootfs := fs.String("rootfs", os.Getenv("FIRECRACKER_ROOTFS"), "root filesystem image path")
	workdir := fs.String("workdir", os.Getenv("FIRECRACKER_WORKDIR"), "base working directory")
	vcpu := fs.Int64("vcpu", 2, "vCPU count")
	mem := fs.Int64("mem", 1024, "memory MiB")
	cid := fs.Uint("cid", 3, "vsock guest CID")
	port := fs.Uint("port", 1024, "guest vsock port")
	bin := fs.String("bin", os.Getenv("FIRECRACKER_BIN"), "firecracker binary path")

	if len(args) == 0 {
		return fmt.Errorf("usage: vmm <start|exec> [vm-id]")
	}
	cmd := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	logger := logrus.New()
	logger.SetOutput(os.Stderr)

	cfg := vmm.Config{
		KernelImage:      *kernel,
		RootFS:           *rootfs,
		WorkDir:          *workdir,
		VCPU:             *vcpu,
		MemSizeMiB:       *mem,
		VsockCID:         uint32(*cid),
		VsockPort:        uint32(*port),
		BinPath:          *bin,
		StartTimeout:     30 * time.Second,
		StopTimeout:      20 * time.Second,
		StopGraceTimeout: 5 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	switch cmd {
	case "start":
		return start(ctx, cfg, logger)
	case "exec":
		return execCmd(ctx, cfg, fs.Args())
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func start(ctx context.Context, cfg vmm.Config, logger *logrus.Logger) error {
	vm, err := vmm.New(cfg, logger.WithField("component", "vmm"))
	if err != nil {
		return err
	}
	defer func() {
		if err := vm.Destroy(context.Background()); err != nil {
			logger.WithError(err).Error("destroy")
		}
	}()

	if err := vm.Start(ctx); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	logger.Infof("VM %s running (pid=%d, state=%s)", vm.ID(), vm.PID(), vm.State())

	// Wait for guest agent readiness over vsock.
	if err := waitForReady(ctx, vm, logger); err != nil {
		return fmt.Errorf("guest not ready: %w", err)
	}
	logger.Info("guest agent ready")

	// Send shutdown request over vsock.
	if err := sendShutdown(ctx, vm, logger); err != nil {
		return fmt.Errorf("shutdown request failed: %w", err)
	}
	logger.Info("shutdown requested")

	// Stop the VM.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer stopCancel()
	if err := vm.Stop(stopCtx); err != nil {
		logger.WithError(err).Warn("stop")
	}

	return nil
}

// vsockConn wraps a net.Conn with length-prefixed JSON framing.
type vsockConn struct {
	net.Conn
}

func (c *vsockConn) sendFrame(data map[string]any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}

	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))

	if _, err := c.Conn.Write(lenBuf[:]); err != nil {
		return err
	}
	_, err = c.Conn.Write(payload)
	return err
}

func (c *vsockConn) readFrame() (map[string]any, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(c.Conn, lenBuf[:]); err != nil {
		return nil, err
	}
	frameLen := binary.BigEndian.Uint32(lenBuf[:])
	if frameLen == 0 || frameLen > 1<<20 {
		return nil, fmt.Errorf("invalid frame length: %d", frameLen)
	}

	payload := make([]byte, frameLen)
	if _, err := io.ReadFull(c.Conn, payload); err != nil {
		return nil, err
	}

	var msg map[string]any
	if err := json.Unmarshal(payload, &msg); err != nil {
		return nil, err
	}
	return msg, nil
}

func (c *vsockConn) readOK() error {
	resp, err := c.readFrame()
	if err != nil {
		return err
	}
	if kind, _ := resp["kind"].(string); kind == "error" {
		return fmt.Errorf("guest error: %v", resp)
	}
	return nil
}

// dialVSock connects to the guest vsock port using the CONNECT handshake.
func dialVSock(ctx context.Context, vm *vmm.VM, port uint32) (*vsockConn, error) {
	raw, err := vm.Vsock().Dial(ctx, port)
	if err != nil {
		return nil, err
	}
	return &vsockConn{Conn: raw}, nil
}

// waitForReady dials the guest vsock, performs the protocol handshake,
// and polls the health endpoint until the guest agent responds.
func waitForReady(ctx context.Context, vm *vmm.VM, logger *logrus.Logger) error {
	deadline, _ := ctx.Deadline()
	if deadline.IsZero() {
		deadline = time.Now().Add(30 * time.Second)
	}
	readyCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	var conn *vsockConn
	var dialErr error
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-readyCtx.Done():
			if dialErr != nil {
				return fmt.Errorf("vsock dial: %w", dialErr)
			}
			return readyCtx.Err()
		case <-ticker.C:
			c, err := dialVSock(readyCtx, vm, 8080)
			if err == nil {
				conn = c
				goto connected
			}
			dialErr = err
		}
	}

connected:
	defer conn.Close()

	// Protocol handshake.
	if err := conn.sendFrame(map[string]any{
		"kind":     "protocol",
		"protocol": "bunn-vsock",
		"version":  float64(1),
	}); err != nil {
		return fmt.Errorf("send protocol: %w", err)
	}
	if err := conn.readOK(); err != nil {
		return fmt.Errorf("protocol handshake: %w", err)
	}

	// Poll health until ready or deadline.
	ticker2 := time.NewTicker(100 * time.Millisecond)
	defer ticker2.Stop()
	for {
		select {
		case <-readyCtx.Done():
			return readyCtx.Err()
		case <-ticker2.C:
			if err := conn.sendFrame(map[string]any{"kind": "health"}); err != nil {
				continue
			}
			resp, err := conn.readFrame()
			if err != nil {
				continue
			}
			if kind, _ := resp["kind"].(string); kind == "health_ok" {
				return nil
			}
		}
	}
}

// sendShutdown dials the guest vsock and sends a shutdown request.
func sendShutdown(ctx context.Context, vm *vmm.VM, logger *logrus.Logger) error {
	conn, err := dialVSock(ctx, vm, 8080)
	if err != nil {
		return fmt.Errorf("vsock dial: %w", err)
	}
	defer conn.Close()

	if err := conn.sendFrame(map[string]any{
		"kind":     "protocol",
		"protocol": "bunn-vsock",
		"version":  float64(1),
	}); err != nil {
		return fmt.Errorf("send protocol: %w", err)
	}
	if err := conn.readOK(); err != nil {
		return fmt.Errorf("protocol handshake: %w", err)
	}

	if err := conn.sendFrame(map[string]any{"kind": "shutdown"}); err != nil {
		return fmt.Errorf("send shutdown: %w", err)
	}
	resp, err := conn.readFrame()
	if err != nil {
		return fmt.Errorf("read shutdown response: %w", err)
	}
	if kind, _ := resp["kind"].(string); kind != "shutting_down" {
		return fmt.Errorf("unexpected shutdown response: %v", resp)
	}
	logger.Info("guest acknowledged shutdown")
	return nil
}

// execCmd executes a command inside a running VM.
// Usage: vmm exec <vm-id> <command> [args...]
func execCmd(ctx context.Context, cfg vmm.Config, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: vmm exec <vm-id> <command> [args...]")
	}
	vmID := args[0]
	command := args[1]
	cmdArgs := args[2:]

	vm, err := vmm.New(cfg, nil)
	if err != nil {
		return fmt.Errorf("create VM: %w", err)
	}
	defer func() {
		_ = vm.Destroy(context.Background())
	}()

	if err := vm.Start(ctx); err != nil {
		return fmt.Errorf("start VM: %w", err)
	}

	// Wait for guest agent readiness.
	if err := waitForReadyExec(ctx, vm); err != nil {
		return fmt.Errorf("guest not ready: %w", err)
	}

	// Send execute request.
	execID := "exec-" + vmID
	timeout := 30 * time.Second

	if err := sendExecute(ctx, vm, execID, command, cmdArgs, nil, "", "", timeout); err != nil {
		return fmt.Errorf("execute: %w", err)
	}

	// Stop the VM after execution.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer stopCancel()
	if err := vm.Stop(stopCtx); err != nil {
		return fmt.Errorf("stop VM: %w", err)
	}

	return nil
}

// waitForReadyExec waits for guest agent readiness (no logging).
func waitForReadyExec(ctx context.Context, vm *vmm.VM) error {
	deadline, _ := ctx.Deadline()
	if deadline.IsZero() {
		deadline = time.Now().Add(30 * time.Second)
	}
	readyCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	var conn *vsockConn
	var dialErr error
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-readyCtx.Done():
			if dialErr != nil {
				return fmt.Errorf("vsock dial: %w", dialErr)
			}
			return readyCtx.Err()
		case <-ticker.C:
			c, err := dialVSock(readyCtx, vm, 8080)
			if err == nil {
				conn = c
				goto connected
			}
			dialErr = err
		}
	}

connected:
	defer conn.Close()

	if err := conn.sendFrame(map[string]any{
		"kind":     "protocol",
		"protocol": "bunn-vsock",
		"version":  float64(1),
	}); err != nil {
		return fmt.Errorf("send protocol: %w", err)
	}
	if err := conn.readOK(); err != nil {
		return fmt.Errorf("protocol handshake: %w", err)
	}

	ticker2 := time.NewTicker(100 * time.Millisecond)
	defer ticker2.Stop()
	for {
		select {
		case <-readyCtx.Done():
			return readyCtx.Err()
		case <-ticker2.C:
			if err := conn.sendFrame(map[string]any{"kind": "health"}); err != nil {
				continue
			}
			resp, err := conn.readFrame()
			if err != nil {
				continue
			}
			if kind, _ := resp["kind"].(string); kind == "health_ok" {
				return nil
			}
		}
	}
}

// sendExecute sends an execute request and streams output to stdout/stderr.
func sendExecute(ctx context.Context, vm *vmm.VM, execID, command string, args []string, env map[string]string, workdir, user string, timeout time.Duration) error {
	conn, err := dialVSock(ctx, vm, 8080)
	if err != nil {
		return fmt.Errorf("vsock dial: %w", err)
	}
	defer conn.Close()

	if err := conn.sendFrame(map[string]any{
		"kind":     "protocol",
		"protocol": "bunn-vsock",
		"version":  float64(1),
	}); err != nil {
		return fmt.Errorf("send protocol: %w", err)
	}
	if err := conn.readOK(); err != nil {
		return fmt.Errorf("protocol handshake: %w", err)
	}

	// Build env map.
	envMap := make(map[string]any)
	for k, v := range env {
		envMap[k] = v
	}

	if err := conn.sendFrame(map[string]any{
		"kind":            "execute",
		"execution_id":    execID,
		"command":         command,
		"args":            args,
		"env":             envMap,
		"working_dir":     workdir,
		"user":            user,
		"timeout_seconds": timeout.Seconds(),
	}); err != nil {
		return fmt.Errorf("send execute: %w", err)
	}

	// Stream output until execution finishes or context is cancelled.
	done := make(chan error, 1)
	go func() {
		for {
			resp, err := conn.readFrame()
			if err != nil {
				done <- err
				return
			}
			kind, _ := resp["kind"].(string)
			switch kind {
			case "execution_started":
				pid, _ := resp["pid"].(float64)
				log.Printf("execution started: id=%s pid=%.0f", execID, pid)
			case "execution_output":
				stream, _ := resp["stream"].(string)
				data, _ := resp["data"].(string)
				if stream == "stderr" {
					os.Stderr.Write([]byte(data))
				} else {
					os.Stdout.Write([]byte(data))
				}
			case "execution_finished":
				exitCode, _ := resp["exit_code"].(float64)
				log.Printf("execution finished: id=%s exit_code=%.0f", execID, exitCode)
				done <- nil
				return
			case "error":
				msg, _ := resp["message"].(string)
				done <- fmt.Errorf("guest error: %s", msg)
				return
			default:
				// Ignore unknown message types.
			}
		}
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
