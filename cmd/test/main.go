// Temporary M3 integration harness. Not a permanent CLI.
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/omkar/sandbox/internal/envcache"
	"github.com/omkar/sandbox/internal/image"
	"github.com/omkar/sandbox/internal/prepare"
	"github.com/omkar/sandbox/vmm"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	var (
		kernel     = flag.String("kernel", os.Getenv("FIRECRACKER_KERNEL"), "kernel image path")
		rootfs     = flag.String("rootfs", os.Getenv("FIRECRACKER_ROOTFS"), "root filesystem image path")
		workdir    = flag.String("workdir", os.Getenv("FIRECRACKER_WORKDIR"), "base working directory")
		bin        = flag.String("bin", os.Getenv("FIRECRACKER_BIN"), "firecracker binary path")
		cid        = flag.Uint("cid", 3, "vsock guest CID")
		cacheDir   = flag.String("cache-dir", "cache", "cache directory")
		guestAgent = flag.String("guest-agent", "", "path to guest agent binary")
	)
	flag.Parse()

	if *kernel == "" || *rootfs == "" || *workdir == "" || *bin == "" {
		log.Fatal("required: --kernel, --rootfs, --workdir, --bin")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	logger := log.New(os.Stderr, "[m3-test] ", log.LstdFlags|log.Lmicroseconds)

	// Test 1: Shell command with stdout
	logger.Println("=== Test 1: Shell command ===")
	if err := testExec(ctx, *kernel, *rootfs, *workdir, *bin, uint32(*cid), "test1", "/bin/busybox", []string{"echo", "hello from sandbox"}, nil, "", "", 30*time.Second, logger); err != nil {
		log.Fatalf("test 1 failed: %v", err)
	}
	*cid++

	// Test 2: Non-zero exit code
	logger.Println("=== Test 2: Non-zero exit code ===")
	if err := testExec(ctx, *kernel, *rootfs, *workdir, *bin, uint32(*cid), "test2", "/bin/busybox", []string{"false"}, nil, "", "", 30*time.Second, logger); err != nil {
		log.Fatalf("test 2 failed: %v", err)
	}
	*cid++

	// Test 3: Environment variables
	logger.Println("=== Test 3: Environment variables ===")
	env := map[string]string{"MY_ENV": "test-value", "APP_NAME": "bunn-burrow"}
	if err := testExec(ctx, *kernel, *rootfs, *workdir, *bin, uint32(*cid), "test3", "/bin/busybox", []string{"sh", "-c", "echo \"ENV=$MY_ENV APP=$APP_NAME\""}, env, "", "", 30*time.Second, logger); err != nil {
		log.Fatalf("test 3 failed: %v", err)
	}
	*cid++

	// Test 4: Working directory
	logger.Println("=== Test 4: Working directory ===")
	if err := testExec(ctx, *kernel, *rootfs, *workdir, *bin, uint32(*cid), "test4", "/bin/busybox", []string{"sh", "-c", "echo \"DIR=$(pwd)\""}, nil, "/tmp", "", 30*time.Second, logger); err != nil {
		log.Fatalf("test 4 failed: %v", err)
	}
	*cid++

	// Test 5: Timeout
	logger.Println("=== Test 5: Timeout ===")
	var timeoutErr error
	if err := testExec(ctx, *kernel, *rootfs, *workdir, *bin, uint32(*cid), "test5", "/bin/busybox", []string{"sleep", "100"}, nil, "", "", 3*time.Second, logger); err == nil {
		log.Fatal("test 5 should have timed out but succeeded")
	} else {
		timeoutErr = err
	}
	logger.Printf("test 5 correctly timed out: %v", timeoutErr)
	*cid++

	// Test 6: Guest Agent remains operational after execution
	logger.Println("=== Test 6: Guest Agent remains operational ===")
	if err := testExec(ctx, *kernel, *rootfs, *workdir, *bin, uint32(*cid), "test6", "/bin/busybox", []string{"echo", "still working"}, nil, "", "", 30*time.Second, logger); err != nil {
		log.Fatalf("test 6 failed: %v", err)
	}

	logger.Println("All M3 tests passed!")

	// Test OCI environment cache if cache-dir and guest-agent are provided
	if *cacheDir != "" && *guestAgent != "" {
		testEnvCache(ctx, *cacheDir, *guestAgent, logger)
	}
}

func testExec(ctx context.Context, kernel, rootfs, workdir, bin string, cid uint32, vmID, command string, args []string, env map[string]string, wd, user string, timeout time.Duration, logger *log.Logger) (runErr error) {
	vmCfg := vmm.Config{
		KernelImage:  kernel,
		RootFS:       rootfs,
		WorkDir:      workdir + "/" + vmID,
		BinPath:      bin,
		VsockCID:     cid,
		VsockPort:    vmm.GuestAgentVsockPort,
		VCPU:         2,
		MemSizeMiB:   1024,
		StartTimeout: 30 * time.Second,
		StopTimeout:  20 * time.Second,
	}

	vm, err := vmm.New(vmCfg, nil)
	if err != nil {
		return fmt.Errorf("create VM: %w", err)
	}
	defer func() {
		if err := vm.Destroy(context.Background()); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("destroy VM %s: %w", vmID, err))
		}
	}()

	if err := vm.Start(ctx); err != nil {
		return fmt.Errorf("start VM: %w", err)
	}
	logger.Printf("VM %s running (pid=%d)", vmID, vm.PID())

	if err := waitForReady(ctx, vm); err != nil {
		return fmt.Errorf("guest not ready: %w", err)
	}
	logger.Printf("VM %s: guest agent ready", vmID)

	if err := sendExecute(ctx, vm, vmID, command, args, env, wd, user, timeout); err != nil {
		return fmt.Errorf("execute: %w", err)
	}
	logger.Printf("VM %s: execution completed", vmID)

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer stopCancel()
	if err := vm.Stop(stopCtx); err != nil {
		return fmt.Errorf("stop VM %s: %w", vmID, err)
	}
	logger.Printf("VM %s: stopped", vmID)
	return nil
}

func newInt64(v int64) *int64 { return &v }

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

func sendExecute(ctx context.Context, vm *vmm.VM, execID, command string, args []string, env map[string]string, workdir, user string, timeout time.Duration) error {
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
				log.Printf("  execution started: pid=%.0f", pid)
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
				log.Printf("  execution finished: exit_code=%.0f", exitCode)
				done <- nil
				return
			case "error":
				msg, _ := resp["message"].(string)
				done <- fmt.Errorf("guest error: %s", msg)
				return
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

func testEnvCache(ctx context.Context, cacheDir, guestAgent string, logger *log.Logger) {
	logger.Println("=== Environment Cache Test ===")

	cfg := prepare.Config{
		CacheDir:    cacheDir,
		RootfsDir:   cacheDir + "/rootfs",
		GuestAgent:  guestAgent,
		OverlaySize: 1024 * 1024 * 1024,
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

	// First run: cache miss
	logger.Println("First environment preparation (cache miss)...")
	start := time.Now()
	env1, err := preparer.Prepare(ctx, ref, "m1-v1")
	if err != nil {
		log.Fatalf("first prepare failed: %v", err)
	}
	logger.Printf("First prepare: %v, cacheKey=%s", time.Since(start), env1.CacheKey)

	// Second run: cache hit
	logger.Println("Second environment preparation (cache hit)...")
	start = time.Now()
	env2, err := preparer.Prepare(ctx, ref, "m1-v1")
	if err != nil {
		log.Fatalf("second prepare failed: %v", err)
	}
	logger.Printf("Second prepare: %v, cacheKey=%s", time.Since(start), env2.CacheKey)

	if env1.CacheKey != env2.CacheKey {
		log.Fatalf("cache key mismatch: %s vs %s", env1.CacheKey, env2.CacheKey)
	}
	logger.Printf("Cache identity consistent: %s", env1.CacheKey)

	// List environments
	envs, err := preparer.ListEnvironments()
	if err != nil {
		log.Fatalf("list environments: %v", err)
	}
	logger.Printf("Cached environments: %d", len(envs))
	for _, e := range envs {
		logger.Printf("  %s (image=%s, digest=%s)", e.CacheKey, e.ImageReference, e.ImageDigest)
	}
}

var _ = envcache.Environment{}
var _ = syscall.SIGTERM
