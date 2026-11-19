# Guest Agent

The Guest Agent is a separate Go module under `guest-agent/`. Its executable is `guest-agent/cmd/guest-agent`; prepared root filesystems install it at `/sbin/bunn-agent`.

## Boot responsibilities

The agent is designed to run as guest PID 1. Startup includes mounting essential filesystems, setting up device/PTY mounts, configuring guest networking from the kernel command line, validating PID 1 behavior, starting process and session managers, and serving the host protocol over vsock.

The bundled agent listens on vsock port `8080`.

## Host/guest operations

The vsock server supports health/readiness, shutdown, execution, and file-transfer requests. Session management supports interactive terminal workflows. The exact message framing and request schemas are implementation details: inspect the current handlers and protocol types before changing or generating messages.

Useful source locations:
- `guest-agent/cmd/guest-agent/main.go`
- `guest-agent/internal/guestagent/`
- `vmm/vsock.go`
- `vmm/sandbox.go`

## Change workflow

When changing the guest protocol:

1. Trace both host-side request construction and guest-side request handling.
2. Check message framing, maximum lengths, request IDs, response behavior, and compatibility with the host.
3. Update both sides together when the wire contract changes.
4. Test malformed messages, partial reads/writes, cancellation, timeout, guest shutdown, and process exit behavior.
5. Use a real Firecracker integration test to verify the full boot-to-request path when the change affects runtime behavior.

Do not treat Firecracker process readiness as Guest Agent readiness. The host must successfully connect to the guest agent's vsock listener.
