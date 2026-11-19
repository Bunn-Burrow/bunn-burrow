---
name: vsock-protocol
description: Use when changing host-to-guest vsock transport, Unix socket handshakes, guest-agent request framing, reconnect behavior, health checks, or snapshot coordination RPCs.
---

# Host/guest vsock protocol

## Understand the two protocol layers

1. Firecracker host-initiated vsock transport: the host dials the Firecracker vsock Unix-domain socket, sends `CONNECT <guest-port>\n`, and expects an `OK <assigned-host-port>\n` acknowledgement.
2. Bunn guest-agent application protocol: after transport setup, the host and guest exchange Bunn protocol messages. Inspect the current framing and message schema before changing it.

Do not confuse the Firecracker UDS path with the guest CID or guest listening port. They represent different parts of the connection.

## Implementation rules

- Keep one host Transport associated with one VM's vsock UDS path.
- Validate non-empty socket path and non-zero guest port.
- Honor context cancellation and use bounded deadlines for connect, handshake, and request/response.
- Close the connection on every handshake or protocol error.
- Treat partial writes, EOF, malformed acknowledgements, and timeouts explicitly.
- Keep request IDs and response correlation where the application protocol supports them.
- Avoid changing wire formats without versioning or backward-compatibility tests.
- Do not assume restored guest listeners need restarting; verify actual listener behavior and reconnect semantics on the target Firecracker/kernel version.
- Do not claim guest-agent readiness based solely on the Firecracker process or API socket being ready.

## Snapshot RPC safety

For guest snapshot operations, verify the request/response contract and state transitions. A successful write is not proof that the guest completed the operation. Freeze/thaw operations must be bounded and observable. A thaw attempt must run on all post-freeze paths, including snapshot errors and cancellation.

## Tests

Use fake net.Conn or local Unix sockets to test handshake success, malformed responses, partial writes, timeout, cancellation, and connection closure. Use a real VM only for the end-to-end host-to-guest validation, and record the exact guest kernel and Firecracker version.
