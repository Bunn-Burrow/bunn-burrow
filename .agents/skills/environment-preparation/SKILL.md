---
name: environment-preparation
description: Prepare reproducible Bunn Burrow guest environments from OCI images. Use when resolving image references, downloading and caching layers, building ext4 root filesystems, injecting the Guest Agent, preparing Python or Node environments, or debugging environment cache misses.
---

# Environment preparation

## When to use

Use this skill to turn an OCI image or supported preset into a Firecracker-ready root filesystem.

## Preparation workflow

Read [Environment preparation](../bunn-burrow/references/environments.md) and [Getting started](../bunn-burrow/references/getting-started.md).

The pipeline in `internal/prepare/` performs the following:

1. Resolve the OCI reference and obtain the immutable image digest.
2. Determine the target platform.
3. Build the environment cache key from image digest, platform, and Guest Agent version.
4. Reuse an existing prepared environment when the cache key matches.
5. Download missing layers into the content-addressed layer cache.
6. Apply image layers to a root filesystem directory.
7. Inject the Guest Agent at `/sbin/bunn-agent` when configured.
8. Finalize filesystem permissions.
9. Build an ext4 image and persist environment metadata.

## Public API

External Go modules should use `pkg/environment`, not internal packages.

- `environment.NewPreparer` creates a preparer with a cache directory and optional Guest Agent.
- `Preparer.Prepare` resolves and prepares an image.
- `GuestAgentVersion` computes a content-based version for the Guest Agent binary.
- `ResolveImageReference` maps the documented Bunn presets to OCI references.

Inspect `pkg/environment/prepare.go` for exact signatures and returned fields before writing integration code.

## Cache behavior

- Cache identity uses the resolved image digest, platform, and Guest Agent version.
- Tags are mutable; use immutable digests when reproducibility matters.
- A changed Guest Agent binary should produce a different environment cache identity.
- Inspect cache metadata and layer digests before deleting or rebuilding caches.
- Do not clear shared caches as a first troubleshooting step.

## Guest compatibility

A prepared rootfs for Guest Agent-backed workflows must contain an executable `/sbin/bunn-agent`. Confirm this before attempting to boot a workload.

Do not assume every OCI image works with the selected kernel or Firecracker configuration. Check architecture, init expectations, filesystem layout, and required binaries.

## Safety

Preparation downloads external image layers and creates filesystem artifacts. Use an explicit cache/work directory and sufficient disk space. Do not run privileged rootfs modification or mount operations without authorization.

## Related references

- [Environment preparation](../bunn-burrow/references/environments.md)
- [Getting started](../bunn-burrow/references/getting-started.md)
- [Guest Agent](../bunn-burrow/references/guest-agent.md)
