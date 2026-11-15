# Bunn Burrow: Product and Operator Guide

This guide describes the current repository and its exposed workflows. A
library API or command flag is not, by itself, evidence that a complete
end-to-end feature is supported. Firecracker-backed tests require explicit
runtime assets; ordinary CI runs unit tests without a user-provided rootfs.

## Architecture

Bunn Burrow is a local Linux application built around Firecracker microVMs.
The host runtime creates and supervises VMs. The guest boots
`/sbin/bunn-agent`, which accepts host requests over vsock and manages guest
processes and terminal sessions.

| Module/path | Responsibility |
| --- | --- |
| Repository root (`github.com/omkar/sandbox`) | Host VMM, image preparation, storage/template APIs, command-line harnesses, and shared packages |
| `guest-agent/` (`sandbox/guest-agent`) | Guest Agent executable and guest-side protocol/process handling |
| `bunn-cli/` (`github.com/Bunn-Burrow/bunn-cli`) | Local `bunn` CLI; it uses the host module directly and is not a remote control-plane client |

There is no persistent host daemon, remote API, or cross-invocation VM
registry. `bunn vm run` and `bunn terminal run` own a foreground VM session.
The root `bunn-burrow start` command boots a VM, waits for Guest Agent
readiness, requests guest shutdown, and destroys the VM; it is not a persistent
VM service. The root `bunn-burrow exec` command boots and destroys a VM around
a workload.

## Feature status

| Area | Current status |
| --- | --- |
| Firecracker VM lifecycle | Host runtime and local CLI workflows are implemented. VM boot requires a compatible kernel, root filesystem, Firecracker binary, and KVM. |
| Guest command execution | Root `bunn-burrow exec` sends a command to the Guest Agent and returns the guest exit status. |
| Interactive terminal | `bunn terminal run` creates a transient VM and attaches a local terminal to a guest shell. The rootfs must include the Guest Agent. |
| OCI rootfs preparation | Preparation tools resolve and cache OCI images, build ext4 root filesystems, and can inject the Guest Agent. First-time preparation needs registry access and host tools. |
| Configuration and host inspection | `bunn config` and `bunn runtime` provide local configuration and capability inspection. Runtime inspection does not boot a VM or prove guest connectivity. |
| Networking | Experimental: optional CLI/API configuration exists, but guest networking and outbound connectivity are not established as working end-to-end workflows. |
| Storage and volumes | Experimental/library-only: Go APIs exist, but the CLI has no volume management commands and guest-side persistent volume formatting/mounting is not verified. |
| Images and templates | Experimental/library-only: manager APIs exist, but image/template management commands are not exposed by `bunn`; template-backed VM creation is not established as a supported workflow. |
| Snapshots and fork/restore | Experimental: snapshot APIs and tests exist, but restore/fork and application- or filesystem-consistent snapshots are not established as end-to-end workflows. |
| Control plane, authentication, authorization | Not implemented as a hosted or remote service. |

Snapshots should be treated as crash-consistent; no filesystem- or
application-consistency guarantee is made. Do not infer production readiness,
multi-tenant isolation guarantees, or feature completeness from package names,
comments, or API presence.

## Requirements

For building and unit tests:

- Go 1.27 or newer.

For VM workflows:

- Linux with accessible `/dev/kvm`.
- Firecracker and a compatible Linux kernel image.
- A root filesystem with `/sbin/bunn-agent` for Guest Agent-backed workflows.

For image preparation, install `e2fsprogs` (`mkfs.ext4` and `debugfs`) and have
access to the selected OCI registry. Some storage paths also require `qemu-img`,
`qemu-nbd`, and available NBD devices. Networking paths require appropriate
Linux bridge/TAP and firewall tools and privileges.

On Debian/Ubuntu, the common host packages can be installed with:

```bash
sudo apt-get update
sudo apt-get install -y e2fsprogs qemu-utils iproute2 nftables
```

This package command does not provide Firecracker, a kernel, KVM access, or
NBD devices. Check local prerequisites before running VM or storage workflows:

```bash
go version
firecracker --version
ls -l /dev/kvm
command -v mkfs.ext4 debugfs qemu-img qemu-nbd ip nft
```

## Build and test

The root Makefile provides:

```bash
make help
make build
make test
make vet
```

`make build` writes named outputs into `bin/`; it replaces any existing files
with those same names. `make clean` removes only the binaries named by the
Makefile, but those names may still refer to local artifacts you want to keep.
Inspect `bin/` before running either target.

To compile each module without producing executable files in the repository:

```bash
GOWORK=off go build ./...
(cd guest-agent && GOWORK=off CGO_ENABLED=0 go build ./...)
(cd bunn-cli && GOWORK=off go build ./...)
```

Use the following for the repository's normal unit-test and vet checks:

```bash
make test
make vet
```

CI checks formatting, dependency integrity, vet, build, unit tests, and race
tests for each Go module. Firecracker integration tests are gated on an
explicit `FIRECRACKER_ROOTFS`; the snapshot/fork integration test additionally
requires `BUNN_RUN_FIRECRACKER_SNAPSHOT_TEST=1`. These tests may boot VMs and
write to the selected rootfs. Use a disposable, explicitly selected test image
and run them only on a host configured for Firecracker.

## Prepare an OCI root filesystem

Build all binaries, then run the preparation harness:

```bash
make build
./bin/bunn-prepare-harness \
  --cache-dir "$HOME/.cache/bunn-burrow" \
  --guest-agent "$PWD/bin/guest-agent-static" \
  --kernel "$HOME/.local/firecracker/vmlinux" \
  --bin "$HOME/.local/bin/firecracker" \
  --workdir "$PWD/bin/prepare-work"
```

The harness prepares its configured image and verifies Guest Agent startup and
shutdown. This is a VM workflow: it needs Firecracker, KVM, a kernel, OCI
registry access, and ext4 utilities. Preserve the cache between runs; a fresh
cache can require substantial downloads and disk space.

The fixed-image utilities `bin/prep_py` and `bin/prep_node` prepare their
configured Python and Node environments. Their cache and Guest Agent can be
selected with `BUNN_CACHE_DIR` and `BUNN_GUEST_AGENT`.

## Commands

The host runtime command is `bunn-burrow` (built as `bin/bunn-burrow`). Its
`start` subcommand checks the boot/readiness/shutdown path:

```bash
FIRECRACKER_BIN="$HOME/.local/bin/firecracker" \
FIRECRACKER_KERNEL="$HOME/.local/firecracker/vmlinux" \
FIRECRACKER_ROOTFS="/path/to/prepared/rootfs.ext4" \
FIRECRACKER_WORKDIR="$PWD/bin/work" \
  ./bin/bunn-burrow start
```

For guest command execution:

```bash
FIRECRACKER_BIN="$HOME/.local/bin/firecracker" \
FIRECRACKER_KERNEL="$HOME/.local/firecracker/vmlinux" \
FIRECRACKER_ROOTFS="/path/to/prepared/rootfs.ext4" \
FIRECRACKER_WORKDIR="$PWD/bin/work" \
  ./bin/bunn-burrow exec demo-vm /bin/busybox echo "hello from the guest"
```

Replace the rootfs path with the image prepared for your environment. The
command accepts a guest executable and arguments; it does not provide flags
for environment variables, stdin, guest working directory, or timeout.
The host `start`, `exec`, and `bunn vm run` workflows use the selected rootfs
directly and may write to it. Use a disposable copy if you need to preserve the
prepared image. `bunn terminal run` creates a writable copy for its transient
session and removes that copy on cleanup.

The `bunn` CLI (built as `bin/bunn`) includes:

```text
bunn config init|view|path
bunn runtime list|inspect
bunn vm run|list|stop|delete
bunn terminal run
```

Cobra also provides the generated `help` and `completion` commands.

Examples:

```bash
./bin/bunn config init
./bin/bunn runtime list
./bin/bunn vm run demo-vm \
  --kernel "$HOME/.local/firecracker/vmlinux" \
  --rootfs "/path/to/prepared/rootfs.ext4" \
  --bin "$HOME/.local/bin/firecracker"
./bin/bunn terminal run \
  --kernel "$HOME/.local/firecracker/vmlinux" \
  --rootfs "/path/to/prepared/rootfs.ext4" \
  --bin "$HOME/.local/bin/firecracker"
```

VM list/stop/delete only operate on VMs tracked in the current CLI process.
There is no daemon to reconnect to a VM after that process exits. The terminal
`--workdir` is the guest working directory; `--vm-workdir` selects host-side
VM resources.

Configuration defaults to `~/.config/bunn-burrow/config.yaml`. CLI flags take
precedence over `BUNN_BURROW_*` environment variables, then the config file,
then defaults. `bunn runtime list` inspects host capabilities; it does not
start a VM.

The terminal can prepare an OCI image when `--image` is selected or no rootfs
is configured; with no configured rootfs or image it defaults to
`bunn/ubuntu:24.04`. Image preparation requires registry access. Networking is
a separate optional path and should not be assumed to work without host
network configuration and end-to-end validation.

The terminal `--secret HOST_ENV_NAME` flag forwards the named host environment
value into the guest. The value is visible to guest processes; do not forward
credentials to an untrusted guest.

## Networking, storage, and snapshots

The terminal and VM commands expose optional networking settings. Enabling
networking can require host privileges and bridge, TAP, and firewall
configuration. Guest IP setup and outbound connectivity are not certified by
this guide.

Storage, volume, image, template, and snapshot operations are primarily Go
APIs rather than user-facing CLI workflows. NBD mapping requires `qemu-nbd`
and available `/dev/nbd*` devices. Host-side attachment alone does not provide
guest filesystem formatting, mounting, or persistence.

Snapshot APIs do not imply filesystem consistency. Restore/fork, snapshot
recovery under failure, and concurrent lifecycle behavior need dedicated
integration validation before relying on them.

## Safety and cleanup

- Treat a guest as untrusted; do not forward host credentials to it.
- This project is not a hosted multi-tenant service and does not provide a
  remote authentication or authorization layer.
- Do not use broad deletion commands against caches, `bin/`, VM work
  directories, root filesystems, or logs.
- Before removing a VM work directory, identify its owner and verify no
  Firecracker process is using it:

  ```bash
  ps -eo pid,args | grep '[f]irecracker'
  ```

- Do not use broad process-kill commands. Stop only a process verified to
  belong to the VM or test you intend to clean up.

The repository's earlier architecture notes and troubleshooting references
remain under `docs/`; this top-level guide is the current starting point.

## Licensing and security reporting

No `LICENSE` file is present, and no license has been selected. Redistribution
terms therefore need a maintainer decision before public distribution.
Likewise, add a responsible-disclosure contact or enable a private reporting
channel before publishing security-reporting instructions; do not send
vulnerability details through public issue trackers.
