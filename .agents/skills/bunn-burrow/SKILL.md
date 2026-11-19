---
name: bunn-burrow
description: Build, prepare, configure, and operate local Firecracker microVM sandboxes with Bunn Burrow. Use when working with the bunn CLI, Go runtime API, OCI environment preparation, guest command execution, terminal sessions, Guest Agent, vsock, VM lifecycle, or runtime troubleshooting.
---

# Bunn Burrow

Bunn Burrow is a local Linux runtime built around Firecracker microVMs. It consists of a Go host runtime, a guest-side agent, OCI-based root filesystem preparation, and the `bunn` CLI. It is not a hosted sandbox API or persistent control plane.

## Identify the workflow

First determine which operation the task requires:

| Task | Read |
|---|---|
| Build, prepare an OCI environment, boot a VM, execute a command, or open a terminal | [Getting started](references/getting-started.md) |
| Use `bunn`, configure runtime paths, or inspect host capabilities | [CLI](references/cli.md) |
| Create or manage a sandbox from Go | [Go API](references/go-api.md) |
| Resolve OCI images, build root filesystems, inject the Guest Agent, or use caches | [Environment preparation](references/environments.md) |
| Change guest startup, process execution, PTY, file transfer, or host/guest messaging | [Guest Agent](references/guest-agent.md) |
| Determine whether networking, volumes, templates, snapshots, or restore are usable | [Feature status](references/feature-status.md) |
| Build, test, debug, or run Firecracker integration tests | [Development](references/development.md) |

Read only the reference needed for the task. Read the implementation and tests before changing behavior; the references explain workflows, not a substitute for the current source.

## Choose the correct execution path

- For a one-off command, use the host runtime's `bunn-burrow exec` workflow or the Go `Sandbox` API.
- For an interactive shell, use `bunn terminal run`.
- For a foreground VM session, use `bunn vm run`.
- For a new guest environment, prepare an OCI image and inject the Guest Agent before boot.
- For Go integrations, use the exported `vmm` package. Do not reach into `internal/` packages from external modules.

Do not invent commands or flags. Check `bunn --help` and the command implementation when the requested option is not in the operator guide.

## Standard workflow

1. Inspect `README.md`, `docs.md`, the relevant reference in this skill, and the source files involved.
2. Check host prerequisites: Linux, KVM, Firecracker, kernel image, root filesystem, and any operation-specific tools.
3. Build the required binaries using the repository Makefile.
4. Prepare or select a root filesystem containing `/sbin/bunn-agent`.
5. Use a disposable writable rootfs for workflows that modify guest storage. Do not assume a run leaves the base image unchanged.
6. Start the VM and wait for Guest Agent readiness before sending guest work.
7. Execute the workload or attach the terminal; capture exit status and errors.
8. Stop the guest and confirm Firecracker has exited before cleaning up VM-owned resources.
9. Report what was actually exercised. Separate unit-test evidence from real KVM/Firecracker integration evidence.

## Runtime and safety boundaries

- Linux with working `/dev/kvm` is required for real microVM execution. Do not treat a successful compile as proof that Firecracker can boot.
- The guest agent runs as PID 1 and handles host requests over vsock. The bundled agent listens on port `8080`.
- The CLI tracks VM handles only within its current process. There is no persistent daemon or cross-invocation VM registry.
- Guest networking, persistent volumes, template-backed workflows, and snapshot restore are not established end-to-end features. See [Feature status](references/feature-status.md) before using these APIs.
- Treat guest workloads as untrusted. Do not pass host credentials or sensitive environment variables into a guest unless the user explicitly requests that exposure and understands the risk.
- Never claim multi-tenant isolation, production readiness, filesystem-consistent snapshots, or unsupported lifecycle guarantees.
- Do not run privileged networking, NBD, firewall, mount, or host-configuration commands without explicit authorization.

## Source map

- Host VM lifecycle and sandbox execution: `vmm/`
- Guest Agent module: `guest-agent/`
- CLI: `bunn-cli/`
- OCI image resolution and preparation: `internal/image/`, `internal/prepare/`, `internal/rootfs/`, `pkg/environment/`
- Storage, templates, images, networking: `internal/storage/`, `internal/template/`, `internal/vmimage/`, `internal/network/`

When documentation and implementation disagree, verify the current code and tests, then correct the documentation as part of the requested change.
