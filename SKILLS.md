# Bunn Burrow Skills

This file is the skill index for AI coding agents working in this repository.

## Skill discovery

Read `AGENTS.md` first for repository-wide rules. Load only the skill needed for the current task. If a task spans multiple areas, load each relevant skill. Do not copy skill instructions into issue descriptions, pull request descriptions, or source files.

## Available skills

| Skill | Load when |
| --- | --- |
| [Issue implementation](.agents/skills/issue-implementation/SKILL.md) | Implementing an issue, fixing a defect, or preparing a focused change. |
| [Go engineering](.agents/skills/go-engineering/SKILL.md) | Writing or reviewing Go code, concurrency, process management, errors, or module boundaries. |
| [Firecracker VMM](.agents/skills/firecracker-vmm/SKILL.md) | Changing Firecracker lifecycle, VM configuration, devices, snapshots, or version compatibility. |
| [Snapshot, restore, and fork](.agents/skills/snapshot-fork-restore/SKILL.md) | Changing snapshot artifacts, restore, fork isolation, consistency, or snapshot recovery. |
| [vsock protocol](.agents/skills/vsock-protocol/SKILL.md) | Changing host/guest transport, framing, handshakes, reconnects, or guest-agent RPCs. |
| [Storage lifecycle](.agents/skills/storage-lifecycle/SKILL.md) | Changing rootfs images, overlays, NBD, qcow2, storage ownership, or cleanup. |
| [Reliability testing](.agents/skills/reliability-testing/SKILL.md) | Adding regression, failure-injection, lifecycle, concurrency, or integration tests. |
| [Security review](.agents/skills/security-review/SKILL.md) | Reviewing isolation, trust boundaries, secrets, privileges, guest input, or host resources. |

## Task routing

- **New feature or bug fix:** load Issue implementation and Go engineering, then load domain-specific skills.
- **Firecracker lifecycle or device change:** load Firecracker VMM; add Snapshot, restore, and fork or vsock protocol when those boundaries are affected.
- **Snapshot or fork change:** load Snapshot, restore, and fork, Storage lifecycle, and Reliability testing. Include Security review when guest identity, secrets, or isolation changes.
- **Guest/host communication:** load vsock protocol, Firecracker VMM, Go engineering, and Security review when trust boundaries change.
- **Disk, image, or cleanup change:** load Storage lifecycle, Reliability testing, and Security review.
- **Concurrency or process supervision:** load Go engineering and Reliability testing; include Firecracker VMM for VM process lifecycle.
- **Security-sensitive change:** load Security review together with the implementation skill for the affected subsystem.
- **Test-only change:** load Reliability testing and the relevant subsystem skill.

## Skill authoring rules

- Keep each skill focused on one engineering domain or repeatable workflow.
- Use YAML frontmatter with a stable `name` and a concise, task-triggering `description`.
- Put operational steps, invariants, failure modes, and verification requirements in the skill.
- Keep repository-wide instructions in `AGENTS.md`; do not duplicate them in every skill.
- Put long API details, examples, and version-specific material in a skill's `references/` directory and link to them from `SKILL.md`.
- Prefer progressive disclosure: the index routes agents to a skill; the skill links to detailed references only when needed.
- Keep instructions specific to the current repository and verify them against the implementation and tests.
- Do not claim experimental behavior is supported without end-to-end evidence.
- Preserve upstream attribution and license terms when incorporating third-party material. Do not copy material whose license or redistribution terms are unclear.
