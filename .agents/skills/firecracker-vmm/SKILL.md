---
name: firecracker-vmm
description: Use when changing the Firecracker host runtime, VMM lifecycle, machine/device configuration, snapshot API calls, process supervision, or version compatibility.
---

# Firecracker VMM engineering

## Establish the actual environment

Before making version-sensitive claims, record:
- Firecracker binary version (`firecracker --version`).
- Go SDK module version from `go.mod`.
- Kernel and architecture used by integration tests.
- Whether `/dev/kvm` is available.
- Whether a test is unit-only or boots a real microVM.

Do not assume a local WSL2 environment behaves like native Linux.

## API discipline

- Consult the official Firecracker documentation and Swagger/OpenAPI spec for the exact binary version in use.
- Compare the Go SDK model and generated client with the versioned REST API. A missing SDK field does not imply the Firecracker API lacks the capability.
- If the SDK cannot express a documented request, isolate any raw HTTP-over-Unix-socket implementation behind the existing API abstraction. Validate status codes, response bodies, context cancellation, and JSON shape.
- Do not silently send undocumented fields or rely on Firecracker ignoring unknown JSON properties.
- Treat snapshot load ordering and device configuration as version-specific. Do not assume a device can be changed before or after restore; verify the exact API lifecycle.
- Keep the Firecracker upgrade decision separate from an SDK/API-client change. Do not upgrade just to work around an SDK model gap.

## Lifecycle invariants

- Distinguish host lifecycle state from Firecracker's reported VM state and process state.
- A VM is not stopped merely because SIGTERM/SIGKILL was sent; wait for confirmed process exit/reap.
- Preserve enough process/API state to clean up resources after partial startup.
- On unexpected exit, transition state and trigger ownership-aware cleanup or make the required operator recovery explicit.
- Make start, stop, destroy, pause, and resume behavior safe to retry where their contracts promise idempotency.

## Device and snapshot rules

- Device configuration order matters. Follow the exact Firecracker API constraints for machine config, boot source, drives, network, vsock, and snapshot load.
- For snapshot restore, inspect all persisted device state and every override supported by the exact Firecracker version.
- Do not infer that a restored VM has a usable host transport just because the guest is running.
- Keep VM API socket and vsock UDS paths unique per Firecracker process.
- Snapshot compatibility across Firecracker versions must be tested; never assume snapshot formats are forward/backward compatible.

## Verification

Use fake API tests for request construction and lifecycle transitions. Use real Firecracker integration tests only with an explicitly selected disposable rootfs and authorized KVM host. Capture the actual Firecracker version and test output in the PR.
