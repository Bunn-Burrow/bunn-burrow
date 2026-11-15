package environment

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Bunn-Burrow/bunn-burrow/internal/image"
	internalprepare "github.com/Bunn-Burrow/bunn-burrow/internal/prepare"
)

// Config contains the storage and Guest Agent settings for OCI preparation.
type Config struct {
	CacheDir    string
	GuestAgent  string
	OverlaySize int64
}

// Prepared describes a rootfs produced by the shared OCI preparation pipeline.
type Prepared struct {
	CacheKey       string
	RootfsPath     string
	ImageReference string
	ImageDigest    string
}

// Preparer exposes the shared OCI preparation pipeline to external Go modules.
type Preparer struct {
	inner    *internalprepare.Preparer
	cacheDir string
}

// NewPreparer creates a preparer backed by Bunn's existing OCI and layer caches.
func NewPreparer(cfg Config) (*Preparer, error) {
	if cfg.CacheDir == "" {
		return nil, fmt.Errorf("cache directory is required")
	}
	inner, err := internalprepare.New(internalprepare.Config{
		CacheDir:    cfg.CacheDir,
		RootfsDir:   filepath.Join(cfg.CacheDir, "rootfs"),
		GuestAgent:  cfg.GuestAgent,
		OverlaySize: cfg.OverlaySize,
	})
	if err != nil {
		return nil, err
	}
	return &Preparer{inner: inner, cacheDir: cfg.CacheDir}, nil
}

// Prepare resolves an OCI reference or local preset and prepares it through the
// existing Bunn image, layer, and environment caches.
func (p *Preparer) Prepare(ctx context.Context, reference, guestAgentVersion string) (*Prepared, error) {
	resolved, err := ResolveImageReference(reference)
	if err != nil {
		return nil, err
	}
	ref, err := image.Parse(resolved)
	if err != nil {
		return nil, fmt.Errorf("parse image reference: %w", err)
	}
	env, err := p.inner.Prepare(ctx, ref, guestAgentVersion)
	if err != nil {
		return nil, err
	}
	return &Prepared{
		CacheKey:       env.CacheKey,
		RootfsPath:     env.RootfsPath,
		ImageReference: env.ImageReference,
		ImageDigest:    env.ImageDigest,
	}, nil
}

// ResolveImageReference maps Bunn workstation presets to OCI references.
// Unrecognized references are passed through to the OCI image pipeline.
func ResolveImageReference(reference string) (string, error) {
	switch reference {
	case "bunn/ubuntu", "bunn/ubuntu:latest", "bunn/ubuntu:24.04":
		return "ubuntu:24.04", nil
	case "bunn/alpine", "bunn/alpine:latest", "bunn/alpine:3.24":
		return "alpine:3.24", nil
	case "bunn/python:3.12":
		return "python:3.12-alpine", nil
	case "bunn/node:20":
		return "node:20-alpine", nil
	case "bunn/go:1.26":
		return "golang:1.26-alpine", nil
	case "bunn/fullstack", "bunn/fullstack:latest":
		return "ubuntu:24.04", nil
	default:
		if reference == "" {
			return "", fmt.Errorf("image reference is required")
		}
		return reference, nil
	}
}

// GuestAgentVersion returns a content-based cache version for a Guest Agent
// binary so rebuilt agents do not reuse rootfs images containing older binaries.
func GuestAgentVersion(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open Guest Agent binary: %w", err)
	}

	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return "", fmt.Errorf("hash Guest Agent binary: %w", err)
	}
	return fmt.Sprintf("sha256:%x", hash.Sum(nil)), nil
}
