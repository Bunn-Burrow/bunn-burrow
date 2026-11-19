---
name: storage-lifecycle
description: Use when changing rootfs images, qcow2 overlays, NBD mappings, qemu-nbd processes, snapshot disk copies, writable layers, or storage cleanup and recovery.
---

# Storage lifecycle and ownership

## Model ownership before coding

For every image, overlay, NBD device, qemu-nbd process, mount, and temporary file, identify:
- Creator and owner.
- Whether it is shared or VM-owned.
- Whether it is read-only or writable.
- Which process may still be using it.
- Normal release path.
- Failure and host-restart recovery path.

Never delete or unmap shared base images. Never release a backing resource while Firecracker or qemu-nbd may still be using it.

## NBD and qcow2 rules

- Treat NBD map/unmap as stateful host operations that can fail partway through.
- Record the exact NBD device and process identity returned by the manager; do not infer ownership from a device number or PID alone.
- Make cleanup idempotent and ownership-aware.
- Verify the mapping is gone after unmap; do not treat a successful command invocation alone as proof of cleanup.
- Keep independent writable overlays for concurrently running forks.
- When flattening qcow2 backing chains, verify the resulting image format and that it no longer depends on mutable parent layers.
- Use temporary output paths and atomic rename where appropriate; clean temporary files on all error paths.

## Recovery

- Reconcile only resources the manager can prove it owns.
- Do not blindly kill qemu-nbd processes or unmap devices based on broad process-name matching.
- Prefer reporting ambiguous orphaned resources for operator intervention over deleting a potentially live resource.
- Aggregate cleanup errors and preserve enough state for a retry.

## Testing

Use temporary files and fake command runners for unit tests. Add failure injection after each acquisition step (create image, start qemu-nbd, attach NBD, launch Firecracker, restore snapshot). Verify cleanup is reverse-order, idempotent, and does not affect shared images or unrelated devices.
