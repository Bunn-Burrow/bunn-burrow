# Go API

Use the exported `vmm` package for Go integrations. The API is implemented in `vmm/`; inspect current exported signatures before writing code because this reference is an orientation guide.

## Sandbox lifecycle

The high-level `vmm.Sandbox` owns a VM lifecycle for a workload. The core flow is:

1. Build a `vmm.Config` with kernel image, rootfs, Firecracker path, work directory, and resource limits.
2. Create the sandbox with `vmm.NewSandbox(cfg)`.
3. Start it with a context and wait for the guest agent readiness check.
4. Execute a `vmm.Workload`, supplying stdout and stderr writers.
5. Close the sandbox and ensure cleanup errors are observed.

Relevant exported types and methods in `vmm/sandbox.go` include:
- `Sandbox`
- `Project`
- `Workload`
- `NewSandbox(Config)`
- `(*Sandbox).Start(context.Context)`
- `(*Sandbox).Execute(context.Context, Workload, io.Writer, io.Writer)`
- `(*Sandbox).CancelExecution(context.Context, string)`
- `(*Sandbox).Close(context.Context)`

The exact `Config` fields and defaults are defined in `vmm/config.go`. `vmm.DefaultConfig()` provides defaults, but callers must still supply the kernel and root filesystem (or a supported template reference).

## Workload shape

A workload is language-agnostic and represents a guest executable plus arguments. It supports:
- Execution ID
- Executable and argument vector
- Environment entries
- Guest working directory
- Stdin reader
- Per-workload timeout

Use an argument vector rather than assembling a shell command string. If shell behavior is required, invoke an explicit guest shell and pass the script as an argument.

## Guest communication

The host connects to the guest through a VM-specific vsock transport. The bundled Guest Agent listens on port `8080`. Use the high-level Sandbox methods unless implementing or debugging the protocol itself.

## OCI environment preparation

For external Go modules, the exported `pkg/environment` package provides `NewPreparer` and `Prepare`. See [Environment preparation](environments.md). Avoid importing `internal/` packages from consumers.

## Implementation checklist

- Propagate context cancellation to start, execute, and close operations.
- Always close the sandbox, including error paths.
- Keep stdout and stderr separate when the caller needs stream identity.
- Check the guest exit code separately from transport or protocol errors.
- Bound workload time and input sizes according to the current implementation.
- Do not assume that process-local runtime state survives program exit.
- Add unit tests for error paths and a separately gated real Firecracker test for guest behavior.
