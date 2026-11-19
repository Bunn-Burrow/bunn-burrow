---
name: go-engineering
description: Use when writing or reviewing Go code in Bunn Burrow, especially concurrency, process lifecycle, API clients, resource ownership, error handling, and multi-module changes.
---

# Go engineering for Bunn Burrow

## Code quality

- Follow the surrounding package's established patterns; avoid broad stylistic rewrites.
- Keep functions small enough to reason about their state transitions and cleanup paths.
- Wrap errors with operation and resource context using `%w`; do not discard errors silently.
- Use `errors.Join` when multiple independent cleanup failures must be reported.
- Propagate `context.Context` through blocking I/O, subprocesses, waits, and API calls. Do not store request contexts in long-lived structs.
- Use structured logs with VM ID, snapshot ID, resource path/device, and operation. Never log secrets or guest application data.
- Avoid global mutable state. Make ownership and concurrency expectations explicit in types and comments.
- Do not add a dependency for functionality already provided by the standard library or an existing project dependency.

## Concurrency

- Protect shared state consistently; document lock ownership and lock ordering.
- Do not hold a mutex across slow I/O, process waits, or callbacks unless the invariant requires it.
- Avoid goroutine-per-waiter patterns that can lose notifications or leak goroutines.
- Use channels, contexts, and state generations with explicit cancellation and closure ownership.
- Test concurrency-sensitive code with `go test -race`; use deterministic synchronization rather than sleeps where possible.

## Processes and resources

- Track process start, signal delivery, exit observation, and reaping separately.
- A successful signal call is not proof that a process exited.
- Do not release backing resources until the process is confirmed terminated or a safe ownership transfer has occurred.
- Cleanup should be idempotent and should preserve shared resources.
- Avoid PID-only ownership assumptions where PID reuse is possible.

## Module boundaries and validation

The repository has root, `guest-agent/`, and `bunn-cli/` Go modules. Run commands in the correct module with `GOWORK=off`.

```bash
GOWORK=off go test ./...
GOWORK=off go vet ./...
GOWORK=off go build ./...
```

For the guest agent:

```bash
cd guest-agent
GOWORK=off CGO_ENABLED=0 go test ./...
GOWORK=off CGO_ENABLED=0 go build ./...
```

Use `gofmt` on changed files. Add tests for behavior, errors, cancellation, and cleanup—not only happy paths.
