# Environment preparation

Bunn Burrow prepares guest root filesystems from OCI images. The implementation is in `internal/image/`, `internal/image/cache/`, `internal/prepare/`, `internal/rootfs/`, and `pkg/environment/`.

## Preparation pipeline

The current pipeline:

1. Resolve the OCI reference to an image.
2. Read its immutable digest and platform.
3. Derive a cache key from image digest, platform, and Guest Agent version.
4. Reuse a cached environment when available.
5. Download missing image layers into the content-addressed layer cache.
6. Apply layers into a root filesystem directory.
7. Inject the Guest Agent at `/sbin/bunn-agent` when configured.
8. Finalize root filesystem permissions.
9. Build an ext4 root filesystem image and store environment metadata.

The Guest Agent version participates in the cache key. Rebuilding the agent should therefore produce a different prepared-environment cache identity.

## Built-in reference mapping

The current `pkg/environment.ResolveImageReference` maps these presets:

| Bunn reference | OCI reference |
|---|---|
| `bunn/ubuntu`, `bunn/ubuntu:latest`, `bunn/ubuntu:24.04` | `ubuntu:24.04` |
| `bunn/alpine`, `bunn/alpine:latest`, `bunn/alpine:3.24` | `alpine:3.24` |
| `bunn/python:3.12` | `python:3.12-alpine` |
| `bunn/node:20` | `node:20-alpine` |
| `bunn/go:1.26` | `golang:1.26-alpine` |
| `bunn/fullstack`, `bunn/fullstack:latest` | `ubuntu:24.04` |

Other non-empty references pass through to the OCI image pipeline. Verify the mapping in source before adding or changing presets.

## Public Go entry point

`pkg/environment.NewPreparer` accepts a cache directory, Guest Agent path, and optional overlay size. `Preparer.Prepare(ctx, reference, guestAgentVersion)` returns the prepared rootfs path, image reference, digest, and cache key.

Other methods expose the environment cache and per-sandbox storage helpers. Read `pkg/environment/prepare.go` for exact signatures.

## Image and cache rules

- Prefer immutable digests for reproducible environments. Tags can resolve to different content over time.
- Do not delete a shared cache to fix a single failed preparation. Inspect the cache key, image digest, layer cache, and environment metadata first.
- Do not assume all OCI images are compatible with the selected kernel or Firecracker configuration.
- Confirm the resulting rootfs contains an executable `/sbin/bunn-agent` before attempting Guest Agent-backed workflows.
- Use disposable output paths when testing image preparation; it creates filesystem artifacts and may download large layers.
