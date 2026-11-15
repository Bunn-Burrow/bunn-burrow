# Bunn Burrow

[![CI](https://github.com/Bunn-Burrow/bunn-burrow/actions/workflows/ci.yml/badge.svg)](https://github.com/Bunn-Burrow/bunn-burrow/actions/workflows/ci.yml)

Bunn Burrow is a Linux development project for running commands in Firecracker
microVMs. It includes a host runtime, a guest-side agent, OCI-based root
filesystem preparation, and a local developer CLI. It is not a hosted service
or a persistent control plane.

## What is included

- A Go host runtime for Firecracker VM lifecycle and guest command execution.
- A Guest Agent that runs in the VM and communicates with the host over
  vsock.
- OCI image preparation and local environment caching.
- The `bunn` CLI for local VM and terminal workflows, configuration, and
  runtime inspection.
- Library APIs for storage, templates, snapshots, and networking; availability
  of an API does not mean every workflow is exposed by the CLI or end-to-end
  verified.

## Requirements

- Linux with working KVM access.
- Go 1.27 or newer.
- Firecracker and a compatible Linux kernel image for VM workflows.
- `e2fsprogs` (`mkfs.ext4`, `debugfs`) and registry access for OCI image
  preparation.
- `qemu-img`, `qemu-nbd`, and host networking tools only for the corresponding
  storage or networking paths.

## Build and validate

From the repository root:

```bash
make build
make test
make vet
```

`make build` writes binaries to `bin/` and replaces any files there with the
same output names. To compile packages without writing executable files into
the repository:

```bash
GOWORK=off go build ./...
(cd guest-agent && GOWORK=off CGO_ENABLED=0 go build ./...)
(cd bunn-cli && GOWORK=off go build ./...)
```

## Getting started

Build the binaries and prepare a root filesystem with the Guest Agent injected:

```bash
make build
./bin/bunn-prepare-harness \
  --cache-dir "$HOME/.cache/bunn-burrow" \
  --guest-agent "$PWD/bin/guest-agent-static" \
  --kernel "$HOME/.local/firecracker/vmlinux" \
  --bin "$HOME/.local/bin/firecracker" \
  --workdir "$PWD/bin/prepare-work"
```

The preparation workflow needs registry access, Firecracker, KVM, a compatible
kernel, and ext4 tools. Use the prepared `rootfs.ext4` path under the cache for
a terminal session:

```bash
./bin/bunn terminal run \
  --kernel "$HOME/.local/firecracker/vmlinux" \
  --rootfs "/path/to/prepared/rootfs.ext4" \
  --bin "$HOME/.local/bin/firecracker"
```

The root filesystem must contain `/sbin/bunn-agent`. Replace the example paths
with paths from your host. See the [product and operator guide](docs.md) for
configuration, commands, limitations, and troubleshooting.

## Project layout

| Path | Purpose |
| --- | --- |
| `vmm/`, `internal/`, `pkg/`, `cmd/` | Host runtime, preparation, shared APIs, and development tools |
| `guest-agent/` | Separate Go module for the guest agent |
| `bunn-cli/` | Separate Go module for the `bunn` CLI |
| `docs.md` | Canonical product, setup, and operator guide |
| `docs/` | Preserved architecture notes, research, and earlier operator documentation |

## Current limitations

VM and CLI lifecycle state is process-local; there is no persistent daemon or
cross-invocation reconnection. Guest networking, persistent guest volumes,
template-backed VM workflows, and snapshot restore are not established as
end-to-end supported workflows. Treat this project as a development runtime,
not as a production multi-tenant security boundary. Do not forward credentials
to untrusted guest workloads.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for the development and validation
workflow. No license has been selected in this repository; do not assume
redistribution terms.
