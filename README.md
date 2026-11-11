# Bunn Burrow

[![CI](https://github.com/omkar/sandbox/actions/workflows/ci.yml/badge.svg)](https://github.com/omkar/sandbox/actions/workflows/ci.yml)

Isolated execution platform for developers and AI agents using Firecracker microVMs and OCI container images.

## Architecture Overview

Bunn Burrow runs arbitrary workloads inside isolated Firecracker microVMs. The platform consists of:

- **Host application** (`bunn-burrow`): Controls VM lifecycle, OCI image management, and workload submission
- **Guest Agent** (`guest-agent`): Runs as PID 1 inside each VM, handles vsock communication and process execution
- **Vsock communication**: Host-to-guest channel using length-prefixed JSON frames
- **OCI image pipeline**: Image resolution, layer caching, rootfs extraction, ext4 image building, environment caching

## Repository Structure

```
bunn-burrow/
├── cmd/
│   ├── prepare/          # Temporary M2 integration harness
│   ├── prep_py/          # Temporary Python environment prep
│   ├── prep_node/        # Temporary Node.js environment prep
│   └── test/             # Temporary M3 integration test harness
├── bunn-cli/                 # Independent CLI module (binary: `bunn`)
│   ├── cmd/bunn/             # CLI entrypoint
│   ├── internal/             # CLI packages (cli, config, output, doctor, buildinfo)
│   ├── Makefile              # Build targets
│   └── README.md             # CLI-specific documentation
├── docs/                 # Human-facing documentation
├── guest-agent/
│   ├── bin/              # Built guest agent binaries
│   ├── cmd/guest-agent/  # Guest Agent PID 1 entry point
│   └── internal/guestagent/  # Agent, vsock server, process manager
├── internal/
│   ├── envcache/         # Prepared environment cache
│   ├── image/            # OCI image resolution and layer caching
│   ├── image/cache/      # Content-addressed layer cache
│   ├── prepare/          # Environment preparation orchestrator
│   ├── rootfs/           # OCI layer extraction and rootfs building
│   ├── storage/          # Per-VM writable layer management
│   └── template/         # Immutable VM template metadata and registration
├── vmm/                  # Firecracker VM lifecycle management
├── main.go               # Host application entry point
├── go.mod
└── go.sum
```

## Prerequisites

- Go 1.27+
- Firecracker binary (v1.0.1 or later)
- Linux kernel with KVM support
- `mkfs.ext4`, `debugfs` (e2fsprogs)
- `qemu-img` (qemu-utils) for M5 copy-on-write backing images and overlays
- `qemu-nbd` for M5 NBD device mapping and M6 snapshots
- `~/.local/bin/firecracker` or `firecracker` in PATH
- `~/.local/firecracker/vmlinux` kernel image

## Quick Start

### 1. Build the host application

```bash
cd /home/omkar/sandbox
go build -o bunn-burrow .
```

### 2. Build the Guest Agent

```bash
cd /home/omkar/sandbox/guest-agent
CGO_ENABLED=0 go build -o bin/guest-agent-static ./cmd/guest-agent
```

### 3. Prepare an OCI environment (first run)

```bash
# Prepare Alpine 3.24 environment
/tmp/bunn-prepare \
  --kernel /home/omkar/.local/firecracker/vmlinux \
  --rootfs /path/to/alpine-rootfs.ext4 \
  --workdir /tmp/bunn-burrow-work \
  --bin /home/omkar/.local/bin/firecracker
```

The prepared environment is cached. Subsequent preparations of the same image are cache hits.

### 5. Build the bunn CLI (optional)

```bash
cd bunn-cli
make build
```

The binary is named `bunn` and provides developer-facing commands for VM lifecycle,
configuration, and runtime diagnostics. See [bunn-cli/README.md](bunn-cli/README.md)
for full CLI documentation.

### 4. Execute a workload

```bash
cd /home/omkar/sandbox
FIRECRACKER_KERNEL=/home/omkar/.local/firecracker/vmlinux \
FIRECRACKER_ROOTFS=/path/to/rootfs.ext4 \
FIRECRACKER_WORKDIR=/tmp/bunn-burrow-work \
FIRECRACKER_BIN=/home/omkar/.local/bin/firecracker \
./bunn-burrow exec test-vm /bin/busybox echo "Hello from Firecracker"
```

## Detailed Documentation

- [docs/setup.md](docs/setup.md) - Setup and dependencies
- [docs/build.md](docs/build.md) - Building host and Guest Agent
- [docs/run.md](docs/run.md) - Running the platform
- [docs/architecture.md](docs/architecture.md) - Architecture details
- [docs/runtime-guide.md](docs/runtime-guide.md) - Running workloads by runtime
- [docs/troubleshooting.md](docs/troubleshooting.md) - Common issues and fixes
- [bunn-cli/README.md](bunn-cli/README.md) - CLI build, configuration, and command reference

## bunn CLI Reference

The `bunn` CLI (in `bunn-cli/`) is an independent Go module providing developer-facing
commands for the local runtime:

| Command | Description |
|---------|-------------|
| `bunn version` | Print CLI version |
| `bunn doctor` | Diagnose runtime health |
| `bunn config init` | Initialize config file |
| `bunn config view` | View effective config |
| `bunn config path` | Print config file path |
| `bunn runtime list` | List runtime capabilities |
| `bunn runtime inspect` | Inspect resolved configuration |
| `bunn vm run` | Start a VM |
| `bunn vm list` | List VMs in this session |
| `bunn vm stop` | Stop a VM |
| `bunn vm delete` | Delete a VM |

All commands support `--output table\|json\|text` and `--color auto\|always\|never`.

## Contributing

We welcome contributions! Please see [CONTRIBUTING.md](CONTRIBUTING.md) for
the full development workflow, coding standards, and pull request process.

### Quick contribution checklist

1. Create a branch from `main` (e.g., `feature/description`)
2. Implement your changes
3. Run local validation:

   ```bash
   gofmt -l .
   go mod verify
   go vet ./...
   go build ./...
   go test -count=1 ./...
   go test -race ./...
   cd guest-agent && GOWORK=off CGO_ENABLED=0 go build ./...
   git diff --check
   ```

4. Commit using Conventional Commits (e.g., `feat:`, `fix:`, `docs:`)
5. Push and open a pull request against `main`

## Current Limitations

- Guest Agent must be manually injected into OCI rootfs during preparation
- No guest networking (vsock only for host↔guest communication)
- Each workload execution boots a new Firecracker VM
- `vmm/sandbox.go` uses a different (incompatible) protocol; use the `exec` command in `main.go` instead

## Milestone 4, 5, 6, 7 & 8: Templates, COW Storage, Snapshot/Restore, Lifecycle Management & Runtime Reliability

**Status: Implemented**

M4 adds reusable, immutable VM templates. M5 replaces the full per-VM rootfs
copy with a copy-on-write storage engine using qcow2 backing images and
per-VM writable overlays. M6 adds Firecracker VM snapshot and restore. M7 adds
persistent storage lifecycle management and crash recovery. M8 adds runtime
reliability, crash-consistent snapshots with guest coordination protocol, and
enhanced crash recovery.

- **Template management**: `internal/template` — register a prepared rootfs as an immutable template (ID, name, base image, rootfs path/format, architecture, checksum, version), with retrieval, listing, SHA256 integrity validation, and safe deletion (blocked while in use).
- **Immutable rootfs**: template rootfs and backing image are never opened for write after creation, are chmod read-only, and the rootfs checksum is verified on every VM start.
- **Copy-on-write storage** (M5): `internal/storage` — one immutable qcow2 backing image per template; one VM-private writable qcow2 overlay per VM; `qemu-img` required. Overlays exposed to Firecracker via NBD (`qemu-nbd`) since Firecracker does not understand qcow2 natively.
- **Per-VM writable filesystem**: each VM created from a template (`Config.TemplateID`) boots from an independent qcow2 overlay via a unique NBD device, cleaned up on VM destroy. Concurrent creation from one template is safe.
- **Snapshot & restore** (M6): `vmm/snapshot.go` — crash-consistent snapshots from paused VMs via Firecracker `CreateSnapshot`/`LoadSnapshot`; self-contained overlay copies for snapshot independence; durable `SnapshotStore` with atomic metadata; `RestoreSnapshot` creates independent VMs with unique host-side resources.
- **Storage lifecycle & crash recovery** (M7): `internal/storage/manager.go`, `internal/storage/metadata.go`, `internal/storage/nbd.go` — persistent storage metadata with reference counting, `Recover` for post-crash reconciliation, `CollectGarbage` (dry-run/exec) for orphan cleanup, `ReconcileNBD` for stale NBD mapping detection, `DeleteSnapshot` for safe snapshot removal.
- **Runtime reliability** (M8): `vmm/snapshot_coordinator.go` — `SnapshotCoordinator` with `crash_consistent` mode; Guest Agent snapshot coordination protocol (v2) with `prepare_snapshot`, `sync_filesystems`, `freeze_filesystems`, `thaw_filesystems`, `snapshot_ready` handlers; context-aware NBD `Map`/`UnmapContext`; process identity verification; improved `Stop()` escalation; structured lifecycle logging; enhanced `Recover()` with interrupted operation detection.
  - **Consistency guarantees**: `crash_consistent` (VM paused at snapshot time; disk writes captured as they exist in host page cache). `filesystem_consistent` is deferred — `sync()` alone does not provide filesystem freezing, and guest-side FIFREEZE support cannot be safely guaranteed across all guest images.
- **Backward compatible**: direct rootfs creation (`Config.RootFS` without `TemplateID`) is preserved. Legacy `Fork`/`restoreVM` API preserved.

See `docs/architecture.md` (M5/M6/M7/M8 sections) and `docs/run.md` (M5/M6/M7/M8 usage) for details.

**Deferred to M9:** automatic VM resumption after crash, `filesystem_consistent` snapshots with safe guest freeze/thaw, application-consistent snapshots, a persistent CLI/SDK, runtime metrics collection.
