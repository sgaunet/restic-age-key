# Code Patterns & Best Practices

## Error Handling

User-facing errors use the `Fatal:` prefix to mirror restic's CLI wording — users grep for it. These are silenced with `//nolint:staticcheck` because staticcheck (rightly) flags capitalized error strings; the lint suppression is deliberate, not an oversight.

```go
// cmd/restic-age-key/main.go
return errors.New("Fatal: Please specify repository location (-r or --repository-file)") //nolint:staticcheck
```

Internal/wrapped errors use lowercase `fmt.Errorf` with `%w`:

```go
return fmt.Errorf("failed to encrypt key with age: %w", err)
```

Context-deadline handling is repeated explicitly around every `exec.CommandContext` call because the wrapped `*exec.ExitError` swallows the deadline cause otherwise:

```go
if ctx.Err() == context.DeadlineExceeded {
    return "", fmt.Errorf("timeout exceeded while decrypting key with age")
}
var exitErr *exec.ExitError
if errors.As(err, &exitErr) {
    return "", fmt.Errorf("%s", string(exitErr.Stderr))
}
```

## Testing Patterns

- One `.txtar` file per scenario in `testdata/`; filename describes the case (`add-bad-recipient.txtar`, `password-plugin-timeout.txtar`).
- Tests exercise the binary end-to-end via `testscript`, never via direct function calls. `package main`, `func TestMain` wires `testscript.Main`.
- `restic` and `age` are invoked as real subprocesses in scripts — there is no mocking layer. CI installs specific versions before running the suite.
- To regenerate expected output: `UPDATE_SCRIPTS=true go test ./...`. Hand-editing `-- expected.txt --` blocks is discouraged because subtle whitespace divergence is easy to miss.

## Subcommand Pattern

Every subcommand follows the same shape: a `runKeyX(ctx, opts, args)` top-level function, wrapped by a `cobra.Command` that applies `options.timeout` via `context.WithTimeout` when non-zero:

```go
RunE: func(cmd *cobra.Command, args []string) error {
    if options.timeout > 0 {
        ctx, cancel := context.WithTimeout(cmd.Context(), options.timeout)
        defer cancel()
        return runKeyAdd(ctx, options, args)
    }
    return runKeyAdd(cmd.Context(), options, args)
},
```

New subcommands should reuse this pattern verbatim.

## Option Resolution

`options` is seeded from environment in `newRootCommand` before flags are bound, so the precedence is **flag > env var > default**. Two env-var patterns matter:

1. **Same-name pairs** (`RESTIC_REPOSITORY` → `--repo`, `RESTIC_PASSWORD` → `--password`) — match restic's own variables for drop-in `RESTIC_PASSWORD_COMMAND` use.
2. **Tool-specific** (`RESTIC_AGE_IDENTITY_FILE`, `RESTIC_AGE_RECIPIENT`, `RESTIC_AGE_TIMEOUT`) — owned by this binary.

`host` and `user` defaults come from `os.Hostname()` and `user.Current()` if unset, so most invocations don't need them.

## Identity Command

`readIdentityCommand` writes the command output to a temp file and rewrites `opts.identityFile` to point at it. Always `defer closeIdentityCommand()` immediately after calling it — leaking the temp file is a security issue because it holds the age private key.

## Go-Specific Conventions

- **Single package, single file** at `cmd/restic-age-key/main.go`. Resist the urge to split early; the file is large but coherent.
- **No interfaces for backends** — they come from `github.com/josh/restic-api` and we use them directly.
- **Comments only when non-obvious.** Per `AGENTS.md`: clarify *why*, not *what*.
- The toolchain (Go, task, golangci-lint, goreleaser) is pinned in `mise.toml`; every CI job installs it via `jdx/mise-action`, so the workflows carry no Go version of their own. `go.mod` has its own `go` directive — keep it ≤ the mise-pinned Go.
