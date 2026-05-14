# Development Workflows

## Feature Development

1. Branch from `main` (current development branch: `feat/initial-version`).
2. Add or modify code in `cmd/restic-age-key/main.go`.
3. Add one `.txtar` scenario per behavior in `testdata/` (happy path, env-var form, flag form, each error path). Look at `testdata/add-flags.txtar` and `testdata/add-envvars.txtar` for the canonical shape.
4. Run the full check locally: `go build ./... && go vet ./... && go test ./...`. Tests need `age` and `restic` binaries on `PATH`.
5. Use `UPDATE_SCRIPTS=true go test ./...` to regenerate the expected-output blocks inside `.txtar` files after a deliberate behavior change — never hand-edit them.
6. Open a PR; CI runs the matrix in `.github/workflows/go.yml`.

## Code Review Process

- All changes go through PR review against `main`.
- Dependabot PRs auto-merge non-major version bumps via `.github/workflows/merge.yml`.
- CI must be green before merge — that means `go build`, `go test`, and `go vet` pass across all matrix entries.

## Testing Strategy

The test layout is unusual and worth understanding before editing:

- `cmd/restic-age-key/main_test.go` — wires `testscript.Main` so the binary built from this package is callable as `restic-age-key` inside scripts, then runs `testscript.Run` over `testdata/`.
- `testdata/*.txtar` — one scenario per file, shell-like script. `exec restic-age-key ...`, `exec restic ...`, and `exec age ...` all run real binaries. Trailing `-- filename --` blocks become files in `$WORK`. `cmp stderr expected.txt` asserts exact output.
- No unit tests exist in the conventional sense — all behavior is exercised end-to-end through the CLI. This is deliberate: the value of this tool is its interop with `restic` and `age`, both of which are real subprocesses in the tests.
- Run a single scenario: `go test -run TestScript/add-flags ./cmd/restic-age-key/` (the suffix matches the `.txtar` filename minus extension; testdata lives at `cmd/restic-age-key/testdata/`).

## Release Process

No automated release pipeline yet. CI (`go.yml`) builds and tests on every push and PR against an `age` × `restic` version matrix:

- age `1.2.1` × restic `0.18.0` (nixos-25.05)
- age `1.2.1` × restic `0.18.1` (nixos-25.11)
- age `1.3.1` × restic `0.18.1` (nixpkgs-unstable)

When changing behavior that touches the `age` CLI surface or restic on-disk format, sanity-check across that matrix. The Go version is pinned to `1.25.5` in both `go.mod` and CI.
