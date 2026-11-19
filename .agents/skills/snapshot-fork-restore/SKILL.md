---
name: snapshot-fork-restore
description: Use when implementing or reviewing snapshot creation, snapshot metadata, restore, fork, parent-child VM independence, vsock overrides, or snapshot consistency.
---

# Snapshot, restore, and fork engineering

## First verify current behavior

Read the current implementations and tests for:
- Legacy snapshot path and SnapshotStore path.
- `restoreVM` and `RestoreSnapshot`.
- Snapshot metadata read/write and validation.
- Overlay copy and NBD mapping ownership.
- Host-to-guest transport setup.
- Guest-agent snapshot RPC handlers.

Audit reports are evidence to investigate, not a substitute for checking the current commit. Comments may be stale; verify versioned Firecracker documentation and live API behavior where safe.

## Independent fork requirements

A child must not share mutable resources with its parent or siblings. Verify:
- Unique VM ID and working directory.
- Unique Firecracker API socket.
- Unique vsock UDS path supported by the selected Firecracker version.
- Correct host-side Transport bound to that child's UDS path.
- Independent writable rootfs/overlay; never allow concurrent writes to a shared mutable image.
- Unique TAP/MAC and network resources if networking is enabled.
- Guest identity, secrets, tokens, UUIDs, clock, and entropy behavior after restore.

Do not assume `vsock_override` changes the guest CID; it is a host UDS path override. Do not invent a post-restore CID reconfiguration sequence. Verify whether the exact API supports the required change at that lifecycle point. For host-initiated UDS connections, document which CID values are actually used and why.

## Snapshot correctness

Distinguish:
- Crash consistency: capture without guest quiescing.
- Filesystem consistency: guest sync and filesystem freeze/thaw.
- Application consistency: application-specific quiesce/transaction coordination.

Never label a snapshot filesystem-consistent unless the host calls the guest protocol, verifies each response, pauses/captures in the correct order, and guarantees thaw on every error/cancellation path.

Treat snapshot publication as a transaction:
1. Create artifacts in a temporary/incomplete state.
2. Validate file existence, type, size, and expected format.
3. Compute and persist integrity metadata.
4. Sync artifact and metadata files as required.
5. Atomically publish metadata and sync the containing directory where required.
6. Mark ready only after all required steps succeed.

On restore, verify integrity before asking Firecracker to load artifacts. Consider that malformed/corrupt snapshot input may terminate the Firecracker process.

## Failure handling

For every create/restore/fork step, enumerate resources acquired so far and define reverse-order cleanup. Cleanup must be idempotent, preserve shared base images, and report errors. Never delete a disk or unmap NBD while a live process may still use it.

## Required tests

- Corrupt and truncated artifacts are rejected before restore.
- Metadata publication failure leaves no ready snapshot.
- Freeze failure does not proceed to snapshot.
- Snapshot failure still thaws the guest.
- Restore failure releases only resources owned by the failed child.
- Parent and at least two children can coexist with independent vsock paths and writable storage.
- Child writes do not leak into parent or sibling filesystems.
