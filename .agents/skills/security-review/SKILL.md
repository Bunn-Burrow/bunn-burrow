---
name: security-review
description: Use when reviewing VM isolation, guest-to-host boundaries, command execution, file transfer, secrets, privileges, networking, process management, or security-sensitive changes in Bunn Burrow.
---

# Bunn Burrow security review

## Threat model

Treat guest workloads and guest-provided data as untrusted. Bunn Burrow runs local commands inside Firecracker microVMs, but the repository does not establish a production multi-tenant security boundary. Do not overstate isolation guarantees.

## Review boundaries

- Host credentials, environment secrets, SSH agents, cloud tokens, and personal files must not be exposed to guest workloads unless an explicit, reviewed feature requires a narrowly scoped transfer.
- Validate all guest-originated paths, identifiers, lengths, message kinds, and archive entries before host-side use.
- Prevent path traversal, symlink escapes, archive bombs, unbounded allocations, and unsafe temporary-file handling.
- Keep privileged host helpers narrow, allowlisted, and explicit about their inputs.
- Avoid shell-string construction from untrusted data; prefer argument arrays and bounded subprocess execution.
- Use least privilege for files, sockets, devices, and helper processes.
- Do not log secrets, tokens, guest payloads, or sensitive command output.
- Verify socket permissions and per-VM ownership; prevent cross-VM transport or file access.
- Ensure cleanup cannot remove shared images, unrelated NBD devices, or resources owned by another process.
- Treat snapshot restore as a security-sensitive clone: review duplicated credentials, machine identity, entropy, and application tokens.

## Required review questions

1. What is the attacker-controlled input?
2. Which host or guest privilege boundary does it cross?
3. Is the input validated before it affects a path, command, device, or API?
4. Can it access another VM's resources?
5. Can it exhaust host CPU, memory, disk, file descriptors, or process slots?
6. Are secrets or private data exposed in logs or guest state?
7. Does failure leave privileged resources behind?

Report concrete exploit paths and evidence. Do not invent vulnerabilities from theoretical concerns alone.
