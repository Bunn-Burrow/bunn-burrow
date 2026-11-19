---
name: sandbox-execution
description: Create and operate Bunn Burrow Firecracker sandboxes from Go. Use when booting a microVM, executing commands or projects, handling stdout and stderr, setting workload timeouts, cancelling execution, or cleaning up sandbox resources.
---

# Sandbox execution

## When to use

Use this skill when a task needs a workload executed inside a Bunn Burrow microVM, either through the exported Go API or the local runtime.

## Go API workflow

Read [Go API](../bunn-burrow/references/go-api.md) and [Getting started](../bunn-burrow/references/getting-started.md) first.

The high-level API is in `vmm/sandbox.go`:

1. Create a `vmm.Config` using `vmm.DefaultConfig()`.
2. Set the kernel image, root filesystem, Firecracker binary, work directory, and any required VM resources.
3. Create the sandbox using `vmm.NewSandbox(cfg)`.
4. Start with a context. Startup waits for the Guest Agent's vsock listener.
5. Execute a `vmm.Workload` with separate stdout and stderr writers.
6. Check both the returned exit code and error. A non-zero guest exit code is not the same as a host transport failure.
7. Call `Close` on every path and handle its error.

Inspect the current exported signatures before generating code. Do not invent methods, options, or protocol fields.

## Workload construction

A workload contains:
- Optional execution ID.
- Executable path and argument vector.
- Environment entries.
- Guest working directory.
- Stdin reader.
- Per-workload timeout.

Prefer an executable plus argument vector. Do not concatenate untrusted values into shell source. If shell semantics are needed, explicitly invoke a guest shell and pass a script argument.

The implementation currently buffers stdin up to `vmm.MaxStdinSize`; it is not an unlimited streaming stdin API. Project uploads are bounded by `vmm.MaxProjectSize` and `vmm.MaxProjectFiles`. Verify limits in source before changing callers.

## Project workflows

For multi-file workloads, inspect the `Project` type and the project/file methods in `vmm/sandbox.go`. Validate relative paths and upload limits on the host before transferring files. Do not assume a project upload is equivalent to a mounted persistent volume.

## Failure handling

- Use caller contexts and explicit workload timeouts.
- Preserve the guest exit code when execution completes.
- Distinguish guest process failure, protocol failure, vsock failure, VM exit, and context cancellation.
- Close connections and sandbox resources on every failure path.
- Do not remove a rootfs or work directory until the Firecracker process is confirmed stopped and the resource is known to belong to this sandbox.

## Validation

Use deterministic unit tests for workload serialization, limits, cancellation, and error paths. Real execution requires Linux/KVM, Firecracker, a compatible kernel, and a rootfs containing `/sbin/bunn-agent`. Use a disposable rootfs for integration tests.

## Related references

- [Go API](../bunn-burrow/references/go-api.md)
- [Guest Agent](../bunn-burrow/references/guest-agent.md)
- [Feature status](../bunn-burrow/references/feature-status.md)
- [Development](../bunn-burrow/references/development.md)
