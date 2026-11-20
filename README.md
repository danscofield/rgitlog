# rgitlog

Lists the commits of a remote Git repository, newest first, without cloning
it. For each commit it prints the SHA, the author's email and the message:

```
<sha> TAB <email> TAB <message>
```

Only commit objects are fetched, using Git's partial-clone filter
(`filter tree:0`), so no file contents or directory trees are downloaded.
Everything received stays in memory, and nothing is written to disk.

## Build

```sh
make            # builds ./rgitlog
make install    # installs to $(go env GOPATH)/bin
```

Requires Go 1.26 or later.

## Usage

```sh
rgitlog git@github.com:owner/repo.git
rgitlog https://github.com/owner/repo.git
rgitlog -n 50 -ref v1.2.0 -json https://github.com/owner/repo.git
```

| Flag | Meaning |
|---|---|
| `-ref NAME` | Branch, tag or full ref to start from (default: the remote's HEAD) |
| `-n N` | Stop after N commits; the server then sends only about that many |
| `-json` | Print one JSON object per line (`sha`, `email`, `message`) |
| `-subject` | Print only the first line of each message |
| `-committer` | Print the committer's email instead of the author's |
| `-ssh-key FILE` | Use this SSH private key |
| `-v` | Print bytes received and object counts to stderr |
| `-timeout D` | Give up after this long (default `10m`) |

In text output, newlines, tabs and backslashes in messages are escaped as
`\n`, `\t` and `\\`, so each commit is exactly one line.

## Authentication

- **SSH:** uses the same keys `ssh` would: ssh-agent's, plus
  `~/.ssh/id_ed25519`, `id_ecdsa` and `id_rsa`. For an encrypted key file, set
  `SSH_KEY_PASSPHRASE`, or pass `-ssh-key`. The host must already be in
  `~/.ssh/known_hosts`.
- **HTTPS:** public repositories need nothing. For private ones, set
  `GIT_TOKEN`, and `GIT_USERNAME` if your host needs one.

## Limitations

- The server must support partial-clone filters. GitHub does. If a server
  doesn't, `rgitlog` exits with an error instead of downloading the full
  repository.
- `IdentityFile` entries in `~/.ssh/config` aren't used. Pass `-ssh-key` for
  keys with other names.
- Without `-n`, every commit in the ref's history is fetched and held in
  memory before anything is printed. That's about 3 KB per commit; the Go
  repository (about 69,000 commits) peaked at about 210 MB.
- `-n N` limits the fetch by depth along every branch of every merge, so
  merge-heavy histories fetch more than N commits while still printing N.
- Order is newest first by committer time, the same as `git log`.
