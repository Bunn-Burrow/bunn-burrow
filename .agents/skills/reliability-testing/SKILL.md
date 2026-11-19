---
name: reliability-testing
description: Use when adding regression tests, concurrency tests, failure injection, lifecycle tests, integration tests, resource leak checks, or CI validation for Bunn Burrow.
---

# Reliability testing

## Test pyramid

1. Unit tests: deterministic logic, state transitions, serialization, checksums, cleanup decisions.
2. Component tests: fake Firecracker API, fake process runner, temporary directories, local Unix sockets.
3. Integration tests: real Firecracker/KVM, guest kernel/rootfs, NBD, and optional networking.
4. Recovery tests: simulated process crashes, partial artifacts, stale mappings, and host restart reconciliation.

Keep privileged integration tests separate from ordinary unit tests.

## Test design

- Reproduce the defect before fixing it.
- Prefer explicit barriers/channels and controlled fakes over sleeps.
- Test success, error, timeout, cancellation, partial failure, repeated cleanup, and concurrent calls.
- Assert externally observable behavior and resource ownership, not private implementation details alone.
- For concurrency changes, run the race detector.
- Avoid tests that require root or host mutation unless they are explicitly gated and documented.
- Do not weaken or skip a test merely to make CI green; explain environment-dependent skips.

## Repository validation

The repository has three Go modules. Use the relevant module directory and `GOWORK=off`.

```bash
make test
make vet
```

CI also performs formatting, dependency verification, build, tests, and race tests for each module. Firecracker snapshot/fork integration is opt-in and needs an explicit rootfs plus `BUNN_RUN_FIRECRACKER_SNAPSHOT_TEST=1`.

Before running a privileged test, inspect the test code to determine whether it boots a VM, writes a rootfs, creates NBD mappings, changes networking, or needs root. Use disposable assets only.

## Evidence in the PR

Record exact commands, exit status, Go version, Firecracker version for integration tests, and any skipped tests with reasons. Never say "all tests pass" when only a subset ran.
