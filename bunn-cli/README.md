# bunn CLI

The `bunn` CLI is the developer-facing command-line interface for the Bunn
Burrow Firecracker microVM sandbox runtime. It operates the local runtime
directly — it is not a Control Plane client.

## Installation

Build from source:

```bash
cd bunn-cli
make build
```

Or install directly:

```bash
cd bunn-cli
make install-cli
```

The binary is named `bunn`.

## Configuration

Configuration is stored at `~/.config/bunn-burrow/config.yaml` by default.
Precedence: CLI flags > environment variables (`BUNN_BURROW_*`) > config file >
defaults.

### Initialize a config file

```bash
bunn config init
```

### View effective configuration

```bash
bunn config view
bunn config view --json
bunn config view --redact
```

### Print config path

```bash
bunn config path
```

## Commands

```text
bunn
├── version          Print CLI version
├── doctor           Diagnose runtime health
├── config           Manage configuration
│   ├── init         Initialize config file
│   ├── view         View effective config
│   └── path         Print config file path
├── runtime          Inspect runtime environment
│   ├── list         List runtime capabilities
│   └── inspect      Inspect resolved configuration
└── vm               Manage VMs (in-process only)
    ├── run          Start a VM
    ├── list         List VMs in this session
    ├── stop         Stop a VM
    └── delete       Delete a VM
```

### Examples

```bash
# Check version
bunn version

# Diagnose runtime health
bunn doctor
bunn doctor --json

# Initialize and view configuration
bunn config init
bunn config view

# Start a VM
bunn vm run my-vm --kernel /path/to/vmlinux --rootfs /path/to/rootfs.ext4

# List VMs in this session
bunn vm list

# Stop a VM
bunn vm stop my-vm

# Delete a VM
bunn vm delete my-vm

# Use JSON output
bunn --output json doctor
bunn --output json vm list
```

## Output Formats

The `--output` flag controls rendering:

| Format  | Description                    |
|---------|--------------------------------|
| table   | Human-readable aligned table   |
| json    | Machine-readable JSON          |
| text    | Plain text                     |

The `--color` flag controls ANSI styling (`auto`, `always`, `never`).
`NO_COLOR` is respected when `auto` is set.

## Environment Variables

| Variable                  | Description                    |
|---------------------------|--------------------------------|
| `BUNN_BURROW_RUNTIME_FIRECRACKER_BIN` | Firecracker binary path     |
| `BUNN_BURROW_RUNTIME_KERNEL_PATH`     | Kernel image path           |
| `BUNN_BURROW_RUNTIME_ROOTFS_PATH`     | Root filesystem path        |
| `BUNN_BURROW_RUNTIME_WORK_DIR`        | VM working directory        |
| `BUNN_BURROW_RUNTIME_START_TIMEOUT`   | Start timeout               |
| `BUNN_BURROW_RUNTIME_STOP_TIMEOUT`    | Stop timeout                |

## Limitations

- VM lifecycle commands only apply to VMs created within the same CLI
  invocation. There is no persistent daemon, so VM state is lost when the CLI
  exits.
- Template and snapshot commands are not yet implemented.
- Application-consistent snapshots are not yet implemented.

## Module

- Module: `github.com/Bunn-Burrow/bunn-cli`
- Go version: 1.27
- Binary: `bunn`
