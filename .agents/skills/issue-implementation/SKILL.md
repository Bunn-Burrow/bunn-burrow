---
name: issue-implementation
description: Use when implementing a Bunn Burrow GitHub issue, fixing an audited defect, or preparing a focused pull request. Enforces issue scope, evidence-based changes, regression tests, and safe repository workflow.
---

# Issue implementation workflow

1. Read the entire issue, linked audit evidence, dependencies, and acceptance criteria.
2. Inspect the working tree and branch before editing. Preserve user changes and never overwrite uncommitted work.
3. Trace the current implementation, all relevant call sites, tests, and module boundaries. Verify whether the reported defect still exists at the current commit.
4. Write a short implementation plan before making changes. Identify invariants, failure paths, and compatibility risks.
5. Implement only the issue's scope. Do not silently expand into unrelated refactors or feature work.
6. Add a regression test that fails before the fix and passes after it. Prefer deterministic unit tests and fake interfaces before privileged integration tests.
7. Run formatting and focused tests, then the affected module's vet/build/tests. Run race tests for concurrency-sensitive code.
8. Inspect `git diff --check`, `git diff --stat`, and the complete diff. Remove unrelated changes.
9. Report what changed, tests actually run, tests not run, and remaining risks. Do not claim an unrun test passed.

## Bunn Burrow-specific constraints

- The project is a local Linux Firecracker runtime, not a hosted control plane.
- Preserve the root, `guest-agent/`, and `bunn-cli/` module boundaries.
- Use `GOWORK=off` for module commands.
- Do not modify or publish local-only `docs/` content.
- Do not run privileged VM, NBD, firewall, sysctl, module-loading, or host-setup commands without explicit authorization.
- Do not upgrade Firecracker or its Go SDK unless the issue proves it necessary.
- Do not claim experimental APIs are end-to-end supported without integration evidence.

## Pull request output

Include: issue link, root cause, design, changed files, regression tests, exact validation commands/results, integration limitations, and follow-up work.
