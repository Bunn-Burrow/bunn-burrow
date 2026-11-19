# Development and validation

The repository has three Go modules: root, `guest-agent/`, and `bunn-cli/`. Use `GOWORK=off` for module commands.

## Normal validation

```bash
make test
make vet
```

Compile without writing binaries into the repository:

```bash
GOWORK=off go build ./...
(cd guest-agent && GOWORK=off CGO_ENABLED=0 go build ./...)
(cd bunn-cli && GOWORK=off go build ./...)
```

The Makefile's `make build` writes named binaries into `bin/`; inspect existing files before running it.

## Test layers

- Unit tests: ordinary Go tests with fakes and temporary directories.
- Component tests: local Unix sockets, fake process runners, image/cache fixtures, and protocol tests.
- Integration tests: real Firecracker, KVM, kernel, and disposable guest rootfs.

Do not run privileged integration tests merely to validate a documentation-only or unrelated change.

## Firecracker integration tests

Integration tests are gated on explicit runtime assets and environment variables. Inspect the test source for the exact gate and required paths before running. The snapshot/fork integration path additionally requires:

```bash
BUNN_RUN_FIRECRACKER_SNAPSHOT_TEST=1
```

Use a disposable rootfs because integration tests may boot VMs and write to the selected image. Record Firecracker version, kernel, rootfs identity, host capabilities, exact command, and result.

## Troubleshooting sequence

1. Confirm the host is Linux and `/dev/kvm` is accessible.
2. Confirm the Firecracker binary and kernel image are readable and compatible.
3. Confirm the rootfs is a valid image and contains `/sbin/bunn-agent`.
4. Inspect the per-VM work directory, API socket, vsock socket, and Firecracker log.
5. Distinguish Firecracker process/API readiness from Guest Agent readiness.
6. Inspect guest-agent logs and host-side vsock errors.
7. Reproduce with a disposable rootfs and a narrow test before changing lifecycle code.

Never claim that a VM booted successfully based only on a successful Go build or a runtime capability inspection.
