# Contributing

Thanks for your interest in contributing to `xredis`.

## Development Setup

1. Install Go (use the version required by `go.mod`).
2. Clone the repository.
3. Run:

```bash
go mod tidy
go test ./...
```

## Pull Request Guidelines

1. Keep changes focused and small.
2. Add/update tests for behavior changes.
3. Update `README.md` for API-visible changes.
4. Update `CHANGELOG.md` under `[Unreleased]`.
5. Ensure CI passes.

## Code Style

- Run `gofmt -w` on modified files.
- Use clear error messages with package context (`xredis: ...`).
- Keep Lua script protocol changes backward-compatible where possible.

## Commit Messages

Recommended format:

```text
type(scope): short summary
```

Examples:

- `feat(limiter): add multi-rule atomic checks`
- `fix(lock): return ErrLockNotHeld on release mismatch`

