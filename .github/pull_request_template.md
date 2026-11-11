<!--
Thank you for contributing to Bunn Burrow! Please read CONTRIBUTING.md before
submitting a pull request.
-->

## Summary

Briefly describe what this pull request does.

## Motivation

Explain why this change is needed. What problem does it solve?

## Implementation Details

Describe the changes made, including any files modified or added.

## Testing Performed

List the validation steps you ran locally:

- [ ] `gofmt -l .` (no output)
- [ ] `go mod verify`
- [ ] `go vet ./...`
- [ ] `go build ./...`
- [ ] `go test -count=1 ./...`
- [ ] `go test -race ./...`
- [ ] Guest Agent build (`cd guest-agent && GOWORK=off CGO_ENABLED=0 go build ./...`)
- [ ] `git diff --check`

## Breaking Changes

Describe any breaking changes, or state "None" if there are none.

## Documentation Updates

List any documentation files updated (e.g., README.md, docs/, CONTRIBUTING.md).

## Checklist

- [ ] My code follows the project's Go formatting conventions
- [ ] I have run all local validation checks
- [ ] I have added or updated tests where applicable
- [ ] I have updated documentation where applicable
- [ ] My commit messages follow Conventional Commits (e.g., `feat:`, `fix:`)
- [ ] My branch is based on an up-to-date `main`
