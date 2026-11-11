package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/omkar/sandbox/internal/image"
	"github.com/omkar/sandbox/internal/prepare"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	cacheDir := "/home/omkar/bunn-burrow/cache"
	guestAgent := "/home/omkar/bunn-burrow/guest-agent/bin/guest-agent-static"

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

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

	ref, err := image.Parse("node:20-alpine")
	if err != nil {
		log.Fatalf("parse reference: %v", err)
	}
	ref.Registry = "docker.io"
	ref.Repository = "library/node"
	ref.Tag = "20-alpine"

	log.Printf("Preparing environment for %s", ref.String())
	start := time.Now()
	env, err := preparer.Prepare(ctx, ref, "m1-v2")
	if err != nil {
		log.Fatalf("prepare environment: %v", err)
	}
	log.Printf("Environment prepared in %v: %s (size=%d bytes)", time.Since(start), env.CacheKey, env.SizeBytes)
	log.Printf("Rootfs path: %s", env.RootfsPath)
}
