# Contributing to Bunn Burrow

Thank you for considering contributing to Bunn Burrow! This document describes
the development workflow, coding standards, and contribution process.

## Development Workflow

### 1. Create a branch

Create a feature branch from an up-to-date `main`:

```bash
git checkout main
git pull origin main
git checkout -b feature/<description>
```

Branch naming conventions:

| Type | Example |
|------|---------|
| New feature | `feature/add-snapshot-coordinator` |
| Bug fix | `fix/storage-reference-leak` |
| Refactor | `refactor/vmm-stop-escalation` |
| Documentation | `docs/update-runbook` |
| Tests | `test/add-storage-recovery-tests` |

### 2. Implement changes

Make your changes following the project's Go coding standards:

- Format code with `gofmt`
- Follow existing naming and package conventions
- Add tests for new functionality
- Update documentation when user-facing behavior changes

### 3. Run local validation

Before pushing, run the full validation suite:

```bash
# Verify Go formatting (should produce no output)
gofmt -l .

# Verify module dependency consistency
go mod verify

# Vet and build
go vet ./...
go build ./...

# Run tests
go test -count=1 ./...
go test -race ./...

# Build the Guest Agent
cd guest-agent && GOWORK=off CGO_ENABLED=0 go build ./...

# Check for whitespace errors
git diff --check
```

### 4. Commit changes

Use [Conventional Commits](https://www.conventionalcommits.org/):

| Type | Example |
|------|---------|
| `feat:` | `feat(storage): add NBD reconciliation` |
| `fix:` | `fix(vmm): improve stop escalation` |
| `refactor:` | `refactor(snapshot): remove global state` |
| `docs:` | `docs: update runbook for M8` |
| `test:` | `test(storage): add GC regression tests` |
| `build:` | `build(ci): add vulnerability scanning` |
| `ci:` | `ci: add race detector job` |
| `chore:` | `chore: update go.sum` |

Example commit message:

```
feat(storage): add NBD reconciliation and process identity verification

Add ReconcileNBD to detect stale qemu-nbd mappings and ProcessIsQemuNBD
to protect against PID reuse. Only mappings owned by this manager are
ever cleaned up.
```

### 5. Push and open a pull request

```bash
git push origin feature/<description>
```

Then open a pull request against `main` on GitHub. Use the pull request
template (`.github/pull_request_template.md`) and ensure all required CI
checks pass before requesting review.

### 6. Review and merge

After review and approval, the pull request can be merged into `main`.
Branch protection requires:

- A pull request before merging
- Required CI status checks to pass
- Branch up to date with `main`
- No force pushes
- No branch deletion

## Coding Standards

- Go code must be formatted with `gofmt`
- Error handling is explicit; no blank `_ = err` without reason
- Structured logging with relevant identifiers (VM ID, snapshot ID, etc.)
- No secrets or guest application data in logs
- Context-aware operations where applicable
- Idempotent cleanup

## Branch Protection Settings (Repository Admin)

The following settings should be enabled on the GitHub repository:

- Require a pull request before merging
- Require status checks to pass before merging
- Require branches to be up to date before merging
- Restrict pushes to `main` (no direct pushes)
- Do not allow force pushes
- Do not allow branch deletion

These settings must be configured manually in the GitHub repository settings.

## Reporting Issues

Use the issue templates in `.github/ISSUE_TEMPLATE/` to report bugs or request
features.

## License

[To be determined]
