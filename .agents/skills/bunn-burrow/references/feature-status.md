# Feature status

This reference records the user-facing status documented by the repository's current operator guide. Re-check `docs.md` and implementation tests before relying on any capability.

| Area | Status |
|---|---|
| Firecracker VM lifecycle | Implemented host runtime and local CLI workflows; requires Linux/KVM, kernel, rootfs, and Firecracker. |
| Guest command execution | Implemented through the Guest Agent; root runtime `exec` sends a command and returns the guest exit status. |
| Interactive terminal | Implemented as a transient VM session. |
| OCI rootfs preparation | Implemented for resolving/caching OCI layers, building ext4 root filesystems, and injecting the Guest Agent. |
| Configuration and host inspection | Implemented; inspection does not boot a VM or prove guest connectivity. |
| Networking | Experimental; guest networking and outbound connectivity are not established as working end-to-end workflows. |
| Storage and volumes | Experimental/library-only; guest-side formatting, mounting, and persistence are not verified. |
| Images and templates | Experimental/library-only; manager APIs exist, but template-backed VM creation is not established as supported. |
| Snapshots and fork/restore | Experimental; restore/fork and filesystem- or application-consistent snapshots are not established end-to-end. |
| Hosted control plane, authentication, authorization | Not implemented. |

## Rules for agents

- Do not infer support from a package, exported type, test stub, or config field.
- Do not describe library-only functionality as a user-facing CLI capability.
- Do not claim snapshot consistency beyond crash consistency.
- Do not claim a persistent daemon, remote API, or cross-invocation VM registry.
- When a feature's status changes, update this reference only after tests demonstrate the new behavior.
