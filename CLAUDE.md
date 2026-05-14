# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Operating Guidelines

**Read `docs/operating-guidelines.md` at the start of every session.** It defines how to plan, verify, and iterate in this repository: plan mode, subagent strategy, verification gates, self-improvement loop, and the communication contract. Treat it as load-bearing context.

## What this is

`restic-age-key` is a Go CLI that lets you unlock a [restic](https://restic.net) repository with an asymmetric [age](https://age-encryption.org/) key instead of a symmetric password. Subcommands: `list`, `add`, `set`, `password`, `from-password`, `repo-init`. See `README.md` for user-facing usage; `AGENTS.md` covers the standard Go build/test/lint commands.

## Common commands

```sh
go build ./...                          # build the binary
go test ./...                           # run all tests (needs `age` and `restic` on PATH)
go test -run TestScript/add-flags ./cmd/restic-age-key/   # run a single testscript scenario (matches cmd/restic-age-key/testdata/add-flags.txtar)
UPDATE_SCRIPTS=true go test ./...       # rewrite expected output blocks inside .txtar files in place
go vet ./... && golangci-lint run ./... # static checks
```

The CI matrix (`.github/workflows/go.yml`) pins Go 1.25.5 and runs against multiple `age` (1.2.1, 1.3.1) and `restic` (0.18.0, 0.18.1) versions — when changing behavior that interacts with either binary, sanity-check it works across that range.

## Testing model — read this before editing tests

Tests are not normal Go tests. `main_test.go` wires `testscript.Main` to call this binary's `main()`, then `TestScript` discovers every `testdata/*.txtar` file and runs each as a shell-like script. Inside a `.txtar`:

- `exec restic-age-key ...` invokes the binary built from this package (no recompile needed).
- `exec restic ...` and `exec age ...` shell out to the real binaries — both must be installed.
- Trailing `-- filename --` blocks are virtual files materialized into `$WORK` before the script runs.
- `cmp stderr file.txt` / `stdout pattern` / `! stderr .` assert exact output. Use `UPDATE_SCRIPTS=true` to regenerate the `-- expected.txt --` blocks rather than hand-editing them after a behavior change.

When adding a feature, the convention is one `.txtar` per scenario (happy path, env-var form, flag form, each error path). Look at `testdata/add-flags.txtar` and `testdata/add-envvars.txtar` for the typical shape.

## Architecture

Everything lives in `cmd/restic-age-key/main.go` (~1200 lines, one file by design). The shape:

1. **`newRootCommand`** wires cobra subcommands and seeds `options{}` from `RESTIC_*` and `RESTIC_AGE_*` env vars before flag parsing. The env-var defaults are intentional — many flows are driven entirely by environment (e.g. `RESTIC_PASSWORD_COMMAND='restic-age-key password'`), so do not introduce flag requirements that would break the env-only path.
2. **Backend resolution** goes through `collectBackends()` → `location.Parse` → factory `Open`/`Create`. Adding a new restic backend means registering it there.
3. **`AgeKey` struct** is the on-disk JSON for each key file in `<repo>/keys/<id>`. It is a standard restic scrypt keyfile (`KDF`, `N`, `R`, `P`, `Salt`, `Data` encrypts the master key) *plus* two extra fields, `AgePubkey` and `AgeData`, holding an age-encrypted random 32-byte password. The same random password is hex-encoded and fed through scrypt to derive the user key that wraps the master key. This is why a recovery path using only `age` + `xxd` works (see README).
4. **`password` flow**: `readPasswordViaIdentity` lists every key file, tries to age-decrypt each `AgeData` blob with the identity, and returns the first one that succeeds — silently skipping `no identity matched any of the recipients` errors. Don't tighten that error handling without thinking about multi-recipient repos.
5. **`add` / `repo-init` flow**: `buildAndSaveAgeKey` calibrates fresh scrypt params (`crypto.Calibrate(500ms, 60)`) for every new key, so two keys for the same repo will have different N/r/p — that's intentional.
6. **`set` flow**: diffs the JSON recipients file against the keys present in the repo, adds missing ones, removes extras. Explicitly refuses to remove the key currently used to open the repo.
7. **`identityCommand`** writes its output to a temp file and rewrites `opts.identityFile` to point at it; the returned `closeCallback` cleans it up. Always `defer closeIdentityCommand()` when calling `readIdentityCommand`.

## Conventions worth keeping

- `//nolint:staticcheck` on the `Fatal:` error strings is deliberate — those messages mirror restic's CLI wording for users who grep error output. Don't reformat them.
- `errors.New`/`fmt.Errorf` with capitalized `Fatal:` prefix is the user-facing error style; lowercase wrapped errors are for internal wrapping.
- New subcommands should follow the existing pattern: top-level `runKeyX` function taking `(ctx, opts, args)`, with a `cobra.Command` that applies `options.timeout` via `context.WithTimeout` when non-zero.

## Documentation

- `docs/operating-guidelines.md` — workflow, verification, self-improvement rules (load-bearing, read every session).
- `docs/architecture.md` — system design and component overview.
- `docs/workflows.md` — feature development, CI, testing strategy.
- `docs/patterns.md` — code patterns specific to this project.
