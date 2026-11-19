# Getting started

## Requirements

For builds and unit tests:
- Go 1.27 or newer.

For VM workflows:
- Linux with working access to `/dev/kvm`.
- Firecracker binary and compatible uncompressed kernel image.
- Root filesystem containing `/sbin/bunn-agent`.

For OCI environment preparation:
- Registry access.
- `e2fsprogs` tools, including `mkfs.ext4` and `debugfs`.

Some storage and network paths additionally need `qemu-img`, `qemu-nbd`, NBD devices, Linux bridge/TAP tools, firewall tools, and privileges.

Check the host before booting:

```bash
go version
firecracker --version
ls -l /dev/kvm
command -v mkfs.ext4 debugfs qemu-img qemu-nbd ip nft
```

## Build

Inspect `bin/` first: `make build` writes named outputs there and replaces files with the same names.

```bash
make build
```

To compile without writing binaries into the repository:

```bash
GOWORK=off go build ./...
(cd guest-agent && GOWORK=off CGO_ENABLED=0 go build ./...)
(cd bunn-cli && GOWORK=off go build ./...)
```

## Prepare an OCI root filesystem

```bash
./bin/bunn-prepare-harness \
  --cache-dir "$HOME/.cache/bunn-burrow" \
  --guest-agent "$PWD/bin/guest-agent-static" \
  --kernel "$HOME/.local/firecracker/vmlinux" \
  --bin "$HOME/.local/bin/firecracker" \
  --workdir "$PWD/bin/prepare-work"
```

The harness resolves the configured OCI image, caches layers, assembles a root filesystem, injects the Guest Agent, builds an ext4 image, and verifies Guest Agent startup and shutdown. Preparation requires registry access, KVM, Firecracker, a compatible kernel, and ext4 tools.

Preserve the cache between runs. A fresh cache can download substantial image layers and consume significant disk space. See [Environment preparation](environments.md) for cache identity and image details.

## Execute a guest command

```bash
FIRECRACKER_BIN="$HOME/.local/bin/firecracker" \
FIRECRACKER_KERNEL="$HOME/.local/firecracker/vmlinux" \
FIRECRACKER_ROOTFS="/path/to/prepared/rootfs.ext4" \
FIRECRACKER_WORKDIR="$PWD/bin/work" \
  ./bin/bunn-burrow exec demo-vm /bin/busybox echo "hello from the guest"
```

The root runtime command accepts a guest executable and arguments. The documented command does not expose environment, stdin, working-directory, or timeout flags. Use the Go API when those workload controls are required.

The `start` command exercises boot/readiness/shutdown and destroys the VM when complete. It is not a persistent service.

## Open an interactive terminal

```bash
./bin/bunn terminal run \
  --kernel "$HOME/.local/firecracker/vmlinux" \
  --rootfs "/path/to/prepared/rootfs.ext4" \
  --bin "$HOME/.local/bin/firecracker"
```

The terminal command can prepare an OCI image when `--image` is selected or when no rootfs is configured. With neither configured, the documented default is `bunn/ubuntu:24.04`.

The terminal creates a writable copy for its transient session and removes that copy during cleanup. Its `--workdir` is the guest working directory; `--vm-workdir` selects host-side VM resources.

## Preserve the base image

The root runtime `start` and `exec` workflows use the selected rootfs directly and may write to it. Use a disposable copy when the prepared base image must remain unchanged. Do not point concurrent VMs at the same writable rootfs.
