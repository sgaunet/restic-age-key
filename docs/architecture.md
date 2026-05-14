# Architecture

## System Overview

`restic-age-key` is a single-binary Go CLI that augments a restic repository's keyfile format with an extra age-encrypted blob. This lets a user unlock the repository with an asymmetric age identity instead of a symmetric passphrase. The binary is also designed to be invoked as a `RESTIC_PASSWORD_COMMAND`, so existing restic tooling treats it as a transparent password provider.

## Components

The entire implementation lives in a single Go package at `cmd/restic-age-key/main.go`. The notable pieces are:

- **`newRootCommand`** — cobra wiring. Builds the `options` struct from `RESTIC_*` / `RESTIC_AGE_*` environment variables before flag parsing, so every subcommand can be driven by env vars, flags, or a mix. Subcommands: `list`, `add`, `set`, `password`, `from-password`, `repo-init`.
- **`collectBackends`** — registers the restic backends consumed via `github.com/josh/restic-api`: local, sftp, rest, s3, gs, azure, b2, swift, rclone. New backends are added here.
- **`AgeKey` struct** — the on-disk JSON format for `<repo>/keys/<id>`. It embeds the standard restic scrypt keyfile fields (`KDF`, `N`, `R`, `P`, `Salt`, `Data`) and adds `AgePubkey` + `AgeData` for the age-encrypted random password.
- **`ageEncryptRandomKey` / `ageDecryptKey`** — shell out to the external `age` binary; never depend on a Go age library.
- **`buildAndSaveAgeKey`** — the core "write a new key file" routine used by `add`, `set`, and `repo-init`. Generates fresh scrypt parameters per key.

## Design Decisions

1. **Single-file, single-package layout.** The project is small and the responsibilities are tightly coupled around one data structure (`AgeKey`); splitting into packages would obscure the flow without reducing complexity.
2. **Shell out to `age` and `rclone` rather than embedding them.** The CI matrix exercises multiple age versions; pinning a Go age library would create version skew with the user's installed `age` binary. The downside is heavier process churn, which is acceptable for an interactive CLI.
3. **Compatible with native restic keyfiles.** The user key derivation (random 32-byte secret → hex → scrypt) means a user with only `age` and standard Unix tools can recover the password without this binary (see README recovery snippet). The age fields are additive; restic itself ignores them.
4. **Per-key scrypt calibration.** `crypto.Calibrate(500ms, 60)` runs at every key creation, so each entry has its own `N`/`r`/`p`. This trades determinism for resistance to brute-force assumptions about parameters.

## Integration Points

- **External binaries**: `age` (required for all crypto operations) and `rclone` (only when the repo URL uses the rclone backend). Paths resolved via `exec.LookPath` at startup, overridable with `--age-program` / `--rclone-program`.
- **Restic backends**: every storage backend supported by `github.com/josh/restic-api/api/backend/*`.
- **Restic itself**: invoked indirectly by acting as `RESTIC_PASSWORD_COMMAND`. Tests in `testdata/` shell out to a real `restic` binary to exercise end-to-end interop.

## Data Flow

**`add` / `repo-init`**: random 32 bytes → hex-encoded as the password → scrypt-KDF derives the user key → user key seals the repo master key → original 32 bytes are age-encrypted to the recipient → both ciphertexts are written together in one `AgeKey` JSON file.

**`password`**: list every key file → for each, age-decrypt `AgeData` with the local identity → first success returns the hex-encoded password to stdout. `no identity matched any of the recipients` is treated as "not for me, try the next one," not an error.

**`set`**: diff the repo's age recipients against a JSON recipients file → add missing → remove extras, refusing to delete the key currently in use to open the repo.
