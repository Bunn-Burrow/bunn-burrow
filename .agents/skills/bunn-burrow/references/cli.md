# CLI reference

The developer CLI is the `bunn` binary built from `bunn-cli/`. It operates the local runtime directly; it is not a remote control-plane client.

## Commands

```text
bunn config init|view|path
bunn runtime list|inspect
bunn vm run|list|stop|delete
bunn terminal run
```

Cobra also provides `help` and `completion`.

Examples:

```bash
bunn config init
bunn config view
bunn runtime list
bunn runtime inspect
bunn vm run demo-vm --kernel /path/to/vmlinux --rootfs /path/to/rootfs.ext4 --bin /path/to/firecracker
bunn terminal run --kernel /path/to/vmlinux --rootfs /path/to/rootfs.ext4 --bin /path/to/firecracker
```

Use `bunn <command> --help` for exact flags and current behavior. Do not infer support for commands that are only represented by Go library packages.

## Configuration precedence

Configuration defaults to:

```text
~/.config/bunn-burrow/config.yaml
```

Precedence is:

1. CLI flags
2. `BUNN_BURROW_*` environment variables
3. Configuration file
4. Built-in defaults

The persistent output options are `--output table|json|text` and `--color auto|always|never`.

## Session-scoped VM state

The CLI's VM tracker is process-local. A VM created by one `bunn` invocation cannot be listed, stopped, or deleted by a later invocation. `bunn vm run` is a foreground session and stops the VM when the command exits. Do not describe this as daemon-managed or persistent VM orchestration.

## Runtime inspection

`bunn runtime list` and `bunn runtime inspect` inspect host capabilities. They do not boot a VM and do not prove that a guest agent is reachable.

## Secrets

The terminal `--secret HOST_ENV_NAME` option forwards the named host environment value into the guest, where guest processes can read it. Never use it for untrusted guest code or imply that the secret remains hidden from the guest.
