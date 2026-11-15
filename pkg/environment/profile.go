package environment

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Bunn-Burrow/bunn-burrow/vmm"
)

const fullstackProfileVersion = "ubuntu-fullstack-v1"

var fullstackPackages = []string{
	"bash-completion",
	"build-essential",
	"ca-certificates",
	"curl",
	"dnsutils",
	"file",
	"git",
	"golang-go",
	"jq",
	"less",
	"make",
	"nodejs",
	"npm",
	"openssh-client",
	"pkg-config",
	"python3",
	"python3-dev",
	"python3-pip",
	"python3-venv",
	"ripgrep",
	"rsync",
	"rustc",
	"cargo",
	"tmux",
	"unzip",
	"vim-tiny",
	"wget",
	"xz-utils",
	"zip",
	"zsh",
}

// BuildFullstack derives a cached Ubuntu developer image from a prepared base.
// The base OCI rootfs is never modified; package installation runs in a VM
// backed by a private copy and the result is cached separately.
func (p *Preparer) BuildFullstack(ctx context.Context, base *Prepared, config vmm.Config, output io.Writer) (_ *Prepared, resultErr error) {
	if p == nil || p.cacheDir == "" {
		return nil, fmt.Errorf("environment preparer is required")
	}
	if base == nil || base.RootfsPath == "" || base.CacheKey == "" {
		return nil, fmt.Errorf("prepared Ubuntu base image is required")
	}
	if output == nil {
		output = io.Discard
	}

	profileDir := filepath.Join(p.cacheDir, "profiles", fullstackProfileKey(base.CacheKey))
	if err := os.MkdirAll(profileDir, 0o750); err != nil {
		return nil, fmt.Errorf("create fullstack profile directory: %w", err)
	}
	lock, err := acquireProfileLock(ctx, filepath.Join(profileDir, "build.lock"))
	if err != nil {
		return nil, fmt.Errorf("lock fullstack profile cache: %w", err)
	}
	defer func() {
		if err := releaseProfileLock(lock); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("release fullstack profile lock: %w", err))
		}
	}()

	rootfsPath := filepath.Join(profileDir, "rootfs.ext4")
	if info, err := os.Stat(rootfsPath); err == nil {
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return nil, fmt.Errorf("cached fullstack rootfs %q is not a non-empty regular file", rootfsPath)
		}
		return &Prepared{
			CacheKey:       fullstackProfileKey(base.CacheKey),
			RootfsPath:     rootfsPath,
			ImageReference: "bunn/fullstack:latest",
			ImageDigest:    base.ImageDigest,
		}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect cached fullstack rootfs: %w", err)
	}
	if config.Network == nil || !config.Network.Enabled {
		return nil, fmt.Errorf("fullstack image provisioning requires guest networking")
	}

	tempPath := filepath.Join(profileDir, "rootfs.building.ext4")
	if err := os.Remove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale fullstack build: %w", err)
	}
	defer func() {
		if err := os.Remove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove incomplete fullstack rootfs: %w", err))
		}
	}()

	copyCmd := exec.CommandContext(ctx, "cp", "--reflink=auto", "--sparse=always", "--preserve=mode", "--", base.RootfsPath, tempPath)
	if outputBytes, err := copyCmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("copy Ubuntu rootfs for fullstack profile: %w: %s", err, string(outputBytes))
	}

	config.RootFS = tempPath
	config.RootFSReadOnly = false
	config.ID = ""
	sandbox, err := vmm.NewSandbox(config)
	if err != nil {
		return nil, fmt.Errorf("create fullstack provisioning VM: %w", err)
	}
	closed := false
	defer func() {
		if closed {
			return
		}
		closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := sandbox.Close(closeCtx); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close fullstack provisioning VM: %w", err))
		}
	}()

	if err := sandbox.Start(ctx); err != nil {
		return nil, fmt.Errorf("start fullstack provisioning VM: %w", err)
	}
	workloads := []vmm.Workload{
		{
			ID:      "fullstack-apt-update",
			Cmd:     "/usr/bin/apt-get",
			Args:    []string{"update"},
			Env:     fullstackEnvironment,
			Dir:     "/",
			Timeout: 15 * time.Minute,
		},
		{
			ID:      "fullstack-apt-install",
			Cmd:     "/usr/bin/apt-get",
			Args:    append([]string{"install", "-y", "--no-install-recommends"}, fullstackPackages...),
			Env:     fullstackEnvironment,
			Dir:     "/",
			Timeout: 30 * time.Minute,
		},
		{
			ID:      "fullstack-apt-clean",
			Cmd:     "/usr/bin/apt-get",
			Args:    []string{"clean"},
			Env:     fullstackEnvironment,
			Dir:     "/",
			Timeout: 2 * time.Minute,
		},
	}
	for _, workload := range workloads {
		exitCode, err := sandbox.Execute(ctx, workload, output, output)
		if err != nil {
			return nil, fmt.Errorf("run %s in fullstack VM: %w", workload.ID, err)
		}
		if exitCode != 0 {
			return nil, fmt.Errorf("%s in fullstack VM exited with status %d", workload.ID, exitCode)
		}
	}

	closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	closeErr := sandbox.Close(closeCtx)
	cancel()
	if closeErr != nil {
		return nil, fmt.Errorf("close fullstack provisioning VM: %w", closeErr)
	}
	closed = true

	if err := os.Rename(tempPath, rootfsPath); err != nil {
		return nil, fmt.Errorf("publish fullstack rootfs: %w", err)
	}

	return &Prepared{
		CacheKey:       fullstackProfileKey(base.CacheKey),
		RootfsPath:     rootfsPath,
		ImageReference: "bunn/fullstack:latest",
		ImageDigest:    base.ImageDigest,
	}, nil
}

var fullstackEnvironment = []string{
	"DEBIAN_FRONTEND=noninteractive",
	"HOME=/root",
	"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
}

func fullstackProfileKey(baseCacheKey string) string {
	material := baseCacheKey + "\x00" + fullstackProfileVersion + "\x00" + strings.Join(fullstackPackages, "\x00")
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(material)))
	return "fullstack-" + sum
}

func acquireProfileLock(ctx context.Context, path string) (*os.File, error) {
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return nil, errors.Join(err, lock.Close())
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), lock.Close())
		case <-ticker.C:
		}
	}
}

func releaseProfileLock(lock *os.File) error {
	unlockErr := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	closeErr := lock.Close()
	return errors.Join(unlockErr, closeErr)
}
