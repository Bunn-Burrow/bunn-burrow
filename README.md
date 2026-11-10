# Bunn Burrow

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
├── docs/                 # Human-facing documentation
├── guest-agent/
│   ├── bin/              # Built guest agent binaries
│   ├── cmd/guest-agent/  # Guest Agent PID 1 entry point
│   └── internal/guestagent/  # Agent, vsock server, process manager
├── internal/
│   ├── envcache/         # Prepared environment cache
│   ├── image/            # OCI image resolution and layer caching
│   ├── prepare/          # Environment preparation orchestrator
│   ├── rootfs/           # OCI layer extraction and rootfs building
│   └── storage/          # Sandbox filesystem isolation
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

## Current Limitations

- No permanent CLI or SDK (temporary harnesses only)
- Guest Agent must be manually injected into OCI rootfs during preparation
- No guest networking (vsock only for host↔guest communication)
- No snapshot or pause/resume in production use
- Each workload execution boots a new Firecracker VM
- `vmm/sandbox.go` uses a different (incompatible) protocol; use the `exec` command in `main.go` instead