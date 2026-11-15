// Temporary M2 integration harness. Not a permanent CLI.
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
	"path/filepath"
	"syscall"
	"time"

	"github.com/Bunn-Burrow/bunn-burrow/internal/envcache"
	"github.com/Bunn-Burrow/bunn-burrow/internal/image"
	"github.com/Bunn-Burrow/bunn-burrow/internal/prepare"
	"github.com/Bunn-Burrow/bunn-burrow/vmm"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	cacheDir := flag.String("cache-dir", envOr("BUNN_CACHE_DIR", "cache"), "environment cache directory")
	guestAgent := flag.String("guest-agent", envOr("BUNN_GUEST_AGENT", filepath.Join("bin", "guest-agent-static")), "Guest Agent binary to inject")
	kernel := flag.String("kernel", envOr("FIRECRACKER_KERNEL", defaultKernelPath()), "kernel image path")
	firecrackerBin := flag.String("bin", envOr("FIRECRACKER_BIN", defaultFirecrackerPath()), "Firecracker binary path")
	workdir := flag.String("workdir", envOr("FIRECRACKER_WORKDIR", filepath.Join("cache", "work")), "VM working directory")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cfg := prepare.Config{
		CacheDir:    *cacheDir,
		RootfsDir:   filepath.Join(*cacheDir, "rootfs"),
		GuestAgent:  *guestAgent,
		OverlaySize: 1024 * 1024 * 1024, // 1 GiB
	}

	preparer, err := prepare.New(cfg)
	if err != nil {
		log.Fatalf("create preparer: %v", err)
	}

	ref, err := image.Parse("alpine:3.24")
	if err != nil {
		log.Fatalf("parse reference: %v", err)
	}
	ref.Registry = "docker.io"
	ref.Repository = "library/alpine"
	ref.Tag = "3.24"

	log.Printf("Preparing environment for %s", ref.String())

	start := time.Now()
	env, err := preparer.Prepare(ctx, ref, "m1-v1")
	if err != nil {
		log.Fatalf("prepare environment: %v", err)
	}
	log.Printf("Environment prepared in %v: %s (size=%d bytes)", time.Since(start), env.CacheKey, env.SizeBytes)

	// List environments
	envs, err := preparer.ListEnvironments()
	if err != nil {
		log.Fatalf("list environments: %v", err)
	}
	log.Printf("Cached environments: %d", len(envs))
	for _, e := range envs {
		log.Printf("  %s (image=%s, digest=%s, size=%d)", e.CacheKey, e.ImageReference, e.ImageDigest, e.SizeBytes)
	}

	// Now create two sandboxes from the same cached environment.
	log.Println("Creating sandbox 1...")
	overlay1, err := preparer.CreateSandboxStorage("sandbox-1")
	if err != nil {
		log.Fatalf("create sandbox 1 storage: %v", err)
	}
	log.Printf("Sandbox 1 overlay: %s", overlay1)

	log.Println("Creating sandbox 2...")
	overlay2, err := preparer.CreateSandboxStorage("sandbox-2")
	if err != nil {
		log.Fatalf("create sandbox 2 storage: %v", err)
	}
	log.Printf("Sandbox 2 overlay: %s", overlay2)

	// Boot sandbox 1 using the prepared environment
	log.Println("Booting sandbox 1...")
	if err := bootAndTestVM(ctx, *kernel, env.RootfsPath, overlay1, *firecrackerBin, *workdir, "vm-1"); err != nil {
		log.Fatalf("sandbox 1 boot/test: %v", err)
	}

	// Boot sandbox 2 using the same prepared environment (cache hit)
	log.Println("Booting sandbox 2 (cache hit)...")
	if err := bootAndTestVM(ctx, *kernel, env.RootfsPath, overlay2, *firecrackerBin, *workdir, "vm-2"); err != nil {
		log.Fatalf("sandbox 2 boot/test: %v", err)
	}

	log.Println("M2 infrastructure verification complete.")
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func defaultKernelPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "firecracker", "vmlinux")
}

func defaultFirecrackerPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "firecracker"
	}
	return filepath.Join(home, ".local", "bin", "firecracker")
}

func vsockCID(vmID string) uint32 {
	if vmID == "vm-1" {
		return 3
	}
	return 4
}

func bootAndTestVM(ctx context.Context, kernel, rootfs, overlay, bin, workdir, vmID string) error {
	vmCfg := vmm.Config{
		KernelImage:  kernel,
		RootFS:       rootfs,
		WorkDir:      filepath.Join(workdir, vmID),
		BinPath:      bin,
		VsockCID:     vsockCID(vmID),
		VsockPort:    vmm.GuestAgentVsockPort,
		VCPU:         2,
		MemSizeMiB:   1024,
		StartTimeout: 30 * time.Second,
		StopTimeout:  20 * time.Second,
	}

	logger := log.New(os.Stderr, fmt.Sprintf("[%s] ", vmID), log.LstdFlags|log.Lmicroseconds)

	vm, err := vmm.New(vmCfg, nil)
	if err != nil {
		return fmt.Errorf("create VM: %w", err)
	}
	defer func() {
		if err := vm.Destroy(context.Background()); err != nil {
			logger.Printf("destroy warning: %v", err)
		}
	}()

	if err := vm.Start(ctx); err != nil {
		return fmt.Errorf("start VM: %w", err)
	}
	logger.Printf("VM running (pid=%d, state=%s)", vm.PID(), vm.State())

	// Wait for guest agent readiness over vsock.
	if err := waitForReady(ctx, vm); err != nil {
		return fmt.Errorf("guest not ready: %w", err)
	}
	logger.Println("guest agent ready")

	// Send shutdown request over vsock.
	if err := sendShutdown(ctx, vm); err != nil {
		return fmt.Errorf("shutdown request: %w", err)
	}
	logger.Println("shutdown requested and acknowledged")

	// Stop the VM (handles graceful shutdown + forced termination escalation).
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer stopCancel()
	if err := vm.Stop(stopCtx); err != nil {
		return fmt.Errorf("stop VM: %w", err)
	}
	logger.Println("VM stopped")
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

func dialVSock(ctx context.Context, vm *vmm.VM, port uint32) (*vsockConn, error) {
	raw, err := vm.Vsock().Dial(ctx, port)
	if err != nil {
		return nil, err
	}
	return &vsockConn{Conn: raw}, nil
}

func waitForReady(ctx context.Context, vm *vmm.VM) error {
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
			c, err := dialVSock(readyCtx, vm, vmm.GuestAgentVsockPort)
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

func sendShutdown(ctx context.Context, vm *vmm.VM) error {
	conn, err := dialVSock(ctx, vm, vmm.GuestAgentVsockPort)
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
	return nil
}

var _ = envcache.Environment{}
var _ = syscall.SIGTERM
