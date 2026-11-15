package prepare

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/Bunn-Burrow/bunn-burrow/internal/envcache"
	"github.com/Bunn-Burrow/bunn-burrow/internal/image"
	"github.com/Bunn-Burrow/bunn-burrow/internal/image/cache"
	"github.com/Bunn-Burrow/bunn-burrow/internal/rootfs"
	"github.com/Bunn-Burrow/bunn-burrow/internal/storage"
)

// Preparer orchestrates OCI image environment preparation: pulling, caching,
// extracting, building rootfs images, and preparing environments for Firecracker.
type Preparer struct {
	imageMgr   *image.Manager
	layerCache *cache.LayerCache
	envCache   *envcache.EnvironmentCache
	storage    *storage.Manager
	rootfsDir  string
	guestAgent string // path to guest agent binary to inject
}

// Config holds configuration for the Preparer.
type Config struct {
	CacheDir    string
	RootfsDir   string
	GuestAgent  string
	OverlaySize int64
}

// New creates a new Preparer.
func New(cfg Config) (*Preparer, error) {
	if err := os.MkdirAll(cfg.CacheDir, 0o750); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	if err := os.MkdirAll(cfg.RootfsDir, 0o750); err != nil {
		return nil, fmt.Errorf("create rootfs dir: %w", err)
	}

	imgMgr := image.NewManager(cfg.CacheDir)
	layerCache := cache.New(filepath.Join(cfg.CacheDir, "layers"))
	if err := layerCache.EnsureDir(); err != nil {
		return nil, err
	}

	envCache := envcache.New(filepath.Join(cfg.CacheDir, "environments"))
	if err := envCache.EnsureDir(); err != nil {
		return nil, err
	}

	storageMgr := storage.New(
		filepath.Join(cfg.CacheDir, "base"),
		filepath.Join(cfg.CacheDir, "overlays"),
		cfg.OverlaySize,
	)
	if err := storageMgr.EnsureDirs(); err != nil {
		return nil, err
	}

	return &Preparer{
		imageMgr:   imgMgr,
		layerCache: layerCache,
		envCache:   envCache,
		storage:    storageMgr,
		rootfsDir:  cfg.RootfsDir,
		guestAgent: cfg.GuestAgent,
	}, nil
}

// Prepare resolves an OCI image, downloads and caches its layers, extracts
// them into a rootfs, injects the Guest Agent, builds an ext4 image, and
// caches the prepared environment.
func (p *Preparer) Prepare(ctx context.Context, ref *image.Reference, guestAgentVersion string) (*envcache.Environment, error) {
	platform := image.Platform()

	// Resolve image first so we have the immutable digest for cache identity.
	img, err := p.imageMgr.Resolve(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("resolve image: %w", err)
	}

	// Get image digest for cache identity (uses resolved digest, not the tag).
	imageDigest, err := img.Digest()
	if err != nil {
		return nil, fmt.Errorf("image digest: %w", err)
	}

	// Cache key is based on resolved digest, platform, and Guest Agent version.
	cacheKey := envcache.CacheKey(imageDigest.String(), platform.String(), guestAgentVersion)

	// Check cache first.
	if cached := p.envCache.Get(cacheKey); cached != nil {
		return cached, nil
	}

	// Begin build (handles concurrent deduplication).
	shouldBuild, finish := p.envCache.BeginBuild(cacheKey)
	if !shouldBuild {
		// Another caller is building; wait for result.
		_ = finish
		cached := p.envCache.Get(cacheKey)
		if cached != nil {
			return cached, nil
		}
		return nil, fmt.Errorf("environment build failed for %s", cacheKey)
	}

	finished := false
	var complete func(error, *envcache.Environment)
	complete = func(err error, env *envcache.Environment) {
		if finished {
			return
		}
		finished = true
		finish(err, env)
	}
	defer complete(nil, nil)

	// Get config digest for cache identity.
	configFile, err := image.Config(img)
	if err != nil {
		complete(fmt.Errorf("read config: %w", err), nil)
		return nil, err
	}
	configRaw, err := json.Marshal(configFile)
	if err != nil {
		complete(fmt.Errorf("marshal config: %w", err), nil)
		return nil, err
	}

	// Download and cache layers.
	layers, err := image.Layers(img)
	if err != nil {
		complete(fmt.Errorf("list layers: %w", err), nil)
		return nil, err
	}

	layerDigests := make([]string, 0, len(layers))
	for _, layer := range layers {
		digest, err := image.LayerDigest(layer)
		if err != nil {
			complete(fmt.Errorf("layer digest: %w", err), nil)
			return nil, err
		}
		layerDigests = append(layerDigests, digest)

		if p.layerCache.Has(digest) {
			continue // already cached
		}

		// Download layer to temp file.
		tmpFile := filepath.Join(p.rootfsDir, "tmp", digest+".tar")
		if err := os.MkdirAll(filepath.Dir(tmpFile), 0o750); err != nil {
			complete(fmt.Errorf("create tmp dir: %w", err), nil)
			return nil, err
		}

		out, err := os.OpenFile(tmpFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			complete(fmt.Errorf("create layer temp: %w", err), nil)
			return nil, err
		}

		if err := image.DownloadLayer(ctx, layer, out); err != nil {
			out.Close()
			os.Remove(tmpFile)
			complete(fmt.Errorf("download layer: %w", err), nil)
			return nil, err
		}
		out.Close()

		// Store in content-addressed cache.
		rc, err := os.Open(tmpFile)
		if err != nil {
			os.Remove(tmpFile)
			complete(fmt.Errorf("open layer temp: %w", err), nil)
			return nil, err
		}

		if err := p.layerCache.Store(digest, rc); err != nil {
			rc.Close()
			os.Remove(tmpFile)
			complete(fmt.Errorf("cache layer: %w", err), nil)
			return nil, err
		}
		rc.Close()
		os.Remove(tmpFile)
	}

	// Extract layers into rootfs directory.
	extractDir := filepath.Join(p.rootfsDir, "extract", cacheKey)
	if err := os.RemoveAll(extractDir); err != nil {
		complete(fmt.Errorf("clean extract dir: %w", err), nil)
		return nil, err
	}
	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		complete(fmt.Errorf("create extract dir: %w", err), nil)
		return nil, err
	}

	builder := rootfs.New(extractDir)
	if err := builder.PrepareDir(); err != nil {
		complete(fmt.Errorf("prepare rootfs dir: %w", err), nil)
		return nil, err
	}

	for _, digest := range layerDigests {
		rc, err := p.layerCache.Get(digest)
		if err != nil {
			complete(fmt.Errorf("get cached layer: %w", err), nil)
			return nil, err
		}

		tmpFile := filepath.Join(p.rootfsDir, "tmp", digest+".extracted")
		out, err := os.OpenFile(tmpFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			rc.Close()
			complete(fmt.Errorf("create extraction temp: %w", err), nil)
			return nil, err
		}
		if _, err := io.Copy(out, rc); err != nil {
			rc.Close()
			out.Close()
			os.Remove(tmpFile)
			complete(fmt.Errorf("copy layer: %w", err), nil)
			return nil, err
		}
		rc.Close()
		out.Close()

		if err := builder.ApplyLayer(tmpFile); err != nil {
			os.Remove(tmpFile)
			complete(fmt.Errorf("apply layer: %w", err), nil)
			return nil, err
		}
		os.Remove(tmpFile)
	}

	// Inject Guest Agent into /sbin/bunn-agent.
	if p.guestAgent != "" {
		if err := injectGuestAgent(extractDir, p.guestAgent); err != nil {
			complete(fmt.Errorf("inject guest agent: %w", err), nil)
			return nil, err
		}
	}
	if err := builder.Finalize(); err != nil {
		complete(fmt.Errorf("finalize rootfs permissions: %w", err), nil)
		return nil, err
	}

	// Build ext4 image from the extracted rootfs.
	safeKey := envcache.SafeCacheKey(cacheKey)
	envDir := filepath.Join(p.storage.BaseDir(), safeKey)
	if err := os.MkdirAll(envDir, 0o750); err != nil {
		complete(fmt.Errorf("create env dir: %w", err), nil)
		return nil, err
	}

	ext4Path := filepath.Join(envDir, "rootfs.ext4")
	if err := buildExt4Image(extractDir, ext4Path); err != nil {
		complete(fmt.Errorf("build ext4 image: %w", err), nil)
		return nil, err
	}

	// Compute size.
	fi, err := os.Stat(ext4Path)
	if err != nil {
		complete(fmt.Errorf("stat ext4: %w", err), nil)
		return nil, err
	}

	env := &envcache.Environment{
		CacheKey:          cacheKey,
		ImageReference:    ref.String(),
		ImageDigest:       imageDigest.String(),
		Platform:          platform.String(),
		RootfsPath:        ext4Path,
		GuestAgentVersion: guestAgentVersion,
		RootfsFormat:      "ext4",
		ConfigDigest:      fmt.Sprintf("%x", configRaw),
		CreatedAt:         time.Now(),
		SizeBytes:         fi.Size(),
	}

	// Store before signaling completion so waiters can read from disk immediately.
	if err := p.envCache.Store(env); err != nil {
		complete(fmt.Errorf("store environment: %w", err), nil)
		return nil, err
	}

	complete(nil, env)
	return env, nil
}

// BaseDir returns the base directory for prepared environments.
func (p *Preparer) BaseDir() string {
	return filepath.Join(p.storage.BasePath())
}

// CreateSandboxStorage creates independent writable storage for a sandbox.
// Returns the path to the overlay ext4 file.
func (p *Preparer) CreateSandboxStorage(sandboxID string) (string, error) {
	return p.storage.CreateOverlay(sandboxID)
}

// RemoveSandboxStorage removes a sandbox's writable storage.
func (p *Preparer) RemoveSandboxStorage(sandboxID string) error {
	return p.storage.RemoveOverlay(sandboxID)
}

// ListEnvironments returns all cached prepared environments.
func (p *Preparer) ListEnvironments() ([]*envcache.Environment, error) {
	return p.envCache.List()
}

// RemoveEnvironment removes a prepared environment from the cache.
func (p *Preparer) RemoveEnvironment(cacheKey string) error {
	return p.envCache.Remove(cacheKey)
}

// injectGuestAgent copies the guest agent binary into the rootfs at /sbin/bunn-agent.
// If /sbin is a symlink (common in Alpine-based images), the symlink is replaced
// with a real directory so the guest agent is placed at a predictable path.
func injectGuestAgent(rootDir, agentPath string) error {
	sbinDir := filepath.Join(rootDir, "sbin")

	// If /sbin is a symlink, remove it and create a real directory.
	if fi, err := os.Lstat(sbinDir); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			if err := os.Remove(sbinDir); err != nil {
				return fmt.Errorf("remove sbin symlink: %w", err)
			}
		}
	}

	if err := os.MkdirAll(sbinDir, 0o755); err != nil {
		return fmt.Errorf("create sbin: %w", err)
	}

	dst := filepath.Join(sbinDir, "bunn-agent")

	in, err := os.Open(agentPath)
	if err != nil {
		return fmt.Errorf("open guest agent: %w", err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("create guest agent in rootfs: %w", err)
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy guest agent: %w", err)
	}
	return nil
}

// buildExt4Image creates an ext4 filesystem image from a directory tree.
func buildExt4Image(srcDir, dstPath string) error {
	// Estimate required size: walk directory and sum file sizes, add 50% headroom.
	var totalSize int64
	err := filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			totalSize += info.Size()
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk source dir: %w", err)
	}
	// Add 50% headroom + 64MB for filesystem overhead.
	imageSize := totalSize*3/2 + 64*1024*1024
	if imageSize < 64*1024*1024 {
		imageSize = 64 * 1024 * 1024 // minimum 64MB
	}
	// Convert to 1K blocks for mkfs.ext4.
	sizeKB := imageSize / 1024

	// Populate directly from the directory tree so names and symlink targets
	// never become commands in a debugfs script.
	cmd := exec.Command("mkfs.ext4", "-q", "-F", "-O", "^has_journal", "-d", srcDir, dstPath, fmt.Sprintf("%d", sizeKB))
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mkfs.ext4: %w\noutput: %s", err, string(output))
	}
	return nil
}
