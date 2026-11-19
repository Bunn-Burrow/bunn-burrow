---
name: bunn-cli
description: Operate the Bunn Burrow local developer CLI. Use when configuring runtime paths, inspecting host capabilities, starting foreground microVM sessions, opening interactive guest terminals, or troubleshooting CLI behavior.
---

# Bunn CLI

## When to use

Use this skill for user-facing local workflows through the `bunn` command. This CLI talks to the local runtime; it is not a hosted API or remote control-plane client.

## Discover commands

Start with:

```bash
bunn --help
bunn <command> --help
```

The currently documented command groups are:

```text
bunn config init|view|path
bunn runtime list|inspect
bunn vm run|list|stop|delete
bunn terminal run
```

Do not infer CLI support from a Go package or manager API. In particular, do not claim there are CLI commands for volumes, image management, templates, snapshot restore, or a persistent daemon unless the command implementation has been added and verified.

## Configuration

Configuration defaults to `~/.config/bunn-burrow/config.yaml`.

Precedence:
1. CLI flags
2. `BUNN_BURROW_*` environment variables
3. Config file
4. Built-in defaults

Output options:
- `--output table|json|text`
- `--color auto|always|never`

Use `bunn config init` to create a starting configuration and `bunn config view` to inspect effective values. Avoid printing secrets in logs or shared terminal output.

## Runtime inspection

`bunn runtime list` and `bunn runtime inspect` report host capabilities and configuration. They do not boot a microVM and do not prove that the Guest Agent is reachable.

## Foreground VM

`bunn vm run` starts a Firecracker VM in the foreground and cleans it up when the process exits or receives an interrupt/termination signal.

The CLI tracks VM objects only inside the current process. A later CLI invocation cannot manage VMs created by an earlier invocation. Do not describe `vm list`, `stop`, or `delete` as persistent daemon operations.

## Interactive terminal

`bunn terminal run` creates a transient VM session and attaches the local terminal to the guest. It supports terminal input/output and terminal resize handling. The terminal workflow can prepare a configured OCI image when needed.

The `--workdir` flag refers to the guest working directory; `--vm-workdir` refers to host-side VM resources. Check `--help` for the exact flags in the current branch.

## Secrets and guest trust

The terminal `--secret HOST_ENV_NAME` option forwards the named host environment variable into the guest. Guest processes can read it. Never use this for untrusted guest workloads.

## Related references

- [CLI](../bunn-burrow/references/cli.md)
- [Getting started](../bunn-burrow/references/getting-started.md)
- [Feature status](../bunn-burrow/references/feature-status.md)
