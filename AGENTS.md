# Bunn Burrow — Agent Instructions

## Project identity and boundaries

Bunn Burrow is a local Linux development runtime for running commands in Firecracker microVMs. It contains a Go host runtime, a guest-side agent, OCI root-filesystem preparation, and a local CLI. It is **not** a hosted service, persistent control plane, or established production multi-tenant security boundary.

Do not describe a library API, package, test stub, or configuration option as an end-to-end supported feature without evidence from a passing integration test. Snapshot restore/fork, guest networking, persistent guest volumes, and template-backed VM workflows must be treated as experimental until their relevant end-to-end tests pass.

Treat guest workloads as untrusted. Never forward host credentials or secrets into untrusted guests. Do not claim production-grade tenant isolation.

## Repository layout

This repository contains three Go modules:

- Root module: host VMM, shared packages, image preparation and harnesses.
- `guest-agent/`: guest-side agent module.
- `bunn-cli/`: local CLI module.

The root module declares Go 1.27. Use `GOWORK=off` for module-specific commands, consistent with CI.

## Mandatory workflow

1. Inspect `git status --short`, current branch, and recent commits before editing. Preserve all pre-existing user changes.
2. Read the complete issue and its acceptance criteria. Work only on that issue; do not bundle unrelated refactors.
3. Inspect the current implementation, call sites, tests, and versioned upstream API documentation before choosing a design.
4. Add regression tests for every bug fix. Prefer deterministic unit tests and fake APIs before privileged integration tests.
5. Format changed Go files with `gofmt`; run the narrow tests first, then the relevant module checks.
6. Report exact commands and results. Distinguish tests actually run from tests merely recommended.
7. Keep the diff small and reviewable. Do not commit, push, merge, or rewrite history unless explicitly asked.

## Validation commands

Repository-wide targets:

```bash
make test
make vet
```

Compile packages without writing binaries into the repository:

```bash
GOWORK=off go build ./...
(cd guest-agent && GOWORK=off CGO_ENABLED=0 go build ./...)
(cd bunn-cli && GOWORK=off go build ./...)
```

The CI workflow also runs formatting, dependency verification, build, unit tests, and race tests for all modules. Run the affected module's tests and race tests where practical.

**Caution:** `make build` writes/replaces named files under `bin/`. Inspect existing local artifacts before running it. Do not delete generated files or caches unless the issue requires it and the user approves.

## Firecracker and privileged tests

- Verify the installed Firecracker version, kernel, rootfs, and `/dev/kvm` before relying on local integration results.
- Firecracker integration tests can boot real VMs and write to the configured rootfs. They are gated by explicit runtime assets; the snapshot/fork test additionally requires `BUNN_RUN_FIRECRACKER_SNAPSHOT_TEST=1`.
- Do not run privileged VM, networking, NBD, firewall, module-loading, sysctl, or host-setup operations without explicit user authorization and a disposable test environment.
- Do not assume WSL2 behaves like a supported native Linux host.
- Do not upgrade Firecracker or the Go SDK as a first response. Verify the exact versioned API capability and compatibility requirement first.
- Firecracker snapshot/restore API behavior must be checked against the versioned official documentation/specification and, when safe, a live API test. Do not trust stale comments over verified API behavior.

## Snapshot and lifecycle invariants

- Never report a snapshot as ready until all required artifacts and metadata are successfully persisted and validated.
- Never claim filesystem or application consistency unless guest/application coordination is implemented and tested.
- A filesystem freeze must always have a guaranteed thaw path, including error and cancellation paths.
- Never release or delete a disk, NBD mapping, socket, or VM resource while a live process may still use it.
- Cleanup should be idempotent, ownership-aware, and should preserve shared base images.
- Restored/forked VMs require independent host-side resources and independently writable storage. Verify vsock path, CID semantics, guest identity, and transport wiring instead of assuming snapshot restore makes them unique.

## Scope and repository hygiene

- Preserve the existing module boundaries and public API compatibility unless the issue explicitly requires a change.
- Do not add dependencies without explaining why the standard library or existing dependencies are insufficient.
- Do not add generated binaries, VM images, local caches, credentials, or machine-specific paths.
- Preserve the local-only `docs/` material: do not modify, stage, delete, move, or publish anything under `docs/` unless Omkar explicitly authorizes it.
- No license has been selected; do not add license headers or claim redistribution terms.
- Keep comments and documentation aligned with tested behavior.

## Pull request expectations

Every PR should include:
- Problem and root cause.
- Design and important trade-offs.
- Tests added and exact validation commands/results.
- Privileged/integration tests not run and why.
- Risks, limitations, and follow-up issues.

A reviewer must be able to map each code change to the issue's acceptance criteria.
