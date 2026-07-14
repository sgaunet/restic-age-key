[![GitHub release](https://img.shields.io/github/release/sgaunet/restic-age-key.svg)](https://github.com/sgaunet/restic-age-key/releases/latest)
![GitHub Downloads](https://img.shields.io/github/downloads/sgaunet/restic-age-key/total)
[![Go Report Card](https://goreportcard.com/badge/github.com/sgaunet/restic-age-key)](https://goreportcard.com/report/github.com/sgaunet/restic-age-key)
![Test Coverage](https://raw.githubusercontent.com/wiki/sgaunet/restic-age-key/coverage-badge.svg)
[![linter](https://github.com/sgaunet/restic-age-key/actions/workflows/linter.yml/badge.svg)](https://github.com/sgaunet/restic-age-key/actions/workflows/linter.yml)
[![coverage](https://github.com/sgaunet/restic-age-key/actions/workflows/coverage.yml/badge.svg)](https://github.com/sgaunet/restic-age-key/actions/workflows/coverage.yml)
[![Snapshot Build](https://github.com/sgaunet/restic-age-key/actions/workflows/snapshot.yml/badge.svg)](https://github.com/sgaunet/restic-age-key/actions/workflows/snapshot.yml)
[![Release Build](https://github.com/sgaunet/restic-age-key/actions/workflows/release.yml/badge.svg)](https://github.com/sgaunet/restic-age-key/actions/workflows/release.yml)
[![GoDoc](https://godoc.org/github.com/sgaunet/restic-age-key?status.svg)](https://godoc.org/github.com/sgaunet/restic-age-key)
[![License](https://img.shields.io/github/license/sgaunet/restic-age-key.svg)](LICENSE)

# restic-age-key

Use asymmetric [age](https://age-encryption.org/) keys instead of a password on your [restic](https://restic.net) repository.

## Usage

### `repo-init`

Create a new restic repository that is unlocked by an age key from the start. Use this instead of `restic init` — there is never a password you have to remember or discard.

First generate an age key pair, if you don't have one already:

```sh
age-keygen -o key.txt
# Public key: age1tmkxjxzan25j6rmjpuffq2ft8z45q75knm356qaypcczvsz4pvds46d9l7
```

Then initialize the repository, passing the **public** key as the recipient:

```sh
restic-age-key repo-init \
  --repo /tmp/restic-repo \
  --recipient age1tmkxjxzan25j6rmjpuffq2ft8z45q75knm356qaypcczvsz4pvds46d9l7

created restic repository 7eba7914 at /tmp/restic-repo

Please note that knowledge of your age identity is required to access
the repository. Losing your identity means that your data is
irrecoverably lost.

repository version: 2
  age key ec29d8a8 for john@foo
```

Only the public key is needed here: `repo-init` generates a random password, initializes the repository with it, wraps the master key for the recipient, and then deletes the temporary password key. Your private identity (`key.txt`) is not read at this point — it is only needed later, to *open* the repository:

```sh
restic-age-key password --repo /tmp/restic-repo --identity-file key.txt
```

Because no password key is left behind, the age identity is the **only** way back into the repository. Back up `key.txt` before you put any data in it.

#### Initializing with several recipients

To grant access to more than one age key at init time, pass a recipients file instead of `--recipient` (the two are mutually exclusive). Each entry gets its own key file in the repository:

```json
[
    { "host": "alice.local", "user": "alice", "pubkey": "age1tmkxjxzan25j6rmjpuffq2ft8z45q75knm356qaypcczvsz4pvds46d9l7" },
    { "host": "bob.local", "user": "bob", "pubkey": "age1g5wluv38nl0vj6p3f7slgq6x4fxexwaqpqt3nctgs9n8jqf6g9wq5ywxfw" }
]
```

```sh
restic-age-key repo-init \
  --repo /tmp/restic-repo \
  --recipients-file recipients.json
```

#### Other flags

| Flag | Env var | Description |
| --- | --- | --- |
| `--repo`, `-r` | `RESTIC_REPOSITORY` | Repository location. Required. |
| `--recipient` | `RESTIC_AGE_RECIPIENT` | Age public key to grant access to. |
| `--recipients-file` | `RESTIC_AGE_RECIPIENTS_FILE` | JSON file of recipients, as above. |
| `--user` | `RESTIC_AGE_USER` | Username recorded on the key. Defaults to the current user. |
| `--host` | `RESTIC_AGE_HOST` | Hostname recorded on the key. Defaults to the current hostname. |
| `--chunker-polynomial` | `RESTIC_AGE_CHUNKER_POLYNOMIAL` | Reuse a repository's chunker polynomial (e.g. `0x3DA3358B4DC173`) so a copy deduplicates against it. Random if unset. |
| `--output` | | Write the new key IDs to a file, one per line. |

### `list`

List keys:

```sh
restic-age-key list \
  --repo /tmp/restic-repo \
  --identity-file key.txt

 ID        Age Pubkey  User  Host  Created
------------------------------------------------------
 abcd1234  age16gj...  john  foo   2025-01-01 12:00:00
 efgh5678  age13er...  john  bar   2025-01-01 12:00:00
 ijkl9012              john  baz   2025-01-01 12:00:00
------------------------------------------------------
```

### `add`

Add an age key to a repository that already exists, using its password (this is the path to take when converting a password-based repository):

```sh
restic-age-key add \
  --repo /tmp/restic-repo \
  --password secret \
  --recipient age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p
```

Add subsequent age keys using another age key:

```sh
restic-age-key add \
  --repo /tmp/restic-repo \
  --identity-file key.txt \
  --recipient age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p
```

### `set`

Update and replace all age keys with a given recipients file:

```sh
restic-age-key set \
  --repo /tmp/restic-repo \
  --recipients-file recipients.json
```

### `password`

This can be used as your `RESTIC_PASSWORD_COMMAND` value.

```sh
restic-age-key password \
  --repo /tmp/restic-repo \
  --identity-file key.txt
```

`restic-age-key` uses the same standard environment variables which allows you to configure your backup scripts using something like:

```
export RESTIC_REPOSITORY=/path/to/repo
export RESTIC_PASSWORD_COMMAND='restic-age-key password'
export RESTIC_AGE_IDENTITY_FILE=/path/to/key.txt

restic backup
```

Should you need to recover your password without `restic-age-key`, you can use a few standard unix tools.

```sh
cat /tmp/restic-repo/keys/123abc | \
  jq --raw-output '."age-data"' | \
  base64 --decode | \
  age --decrypt --identity "your-age-identity.txt" | \
  xxd --plain --cols 64
```

## Credits

The original source code for this project has been retrieved from [https://github.com/josh/restic-age-key](https://github.com/josh/restic-age-key).
