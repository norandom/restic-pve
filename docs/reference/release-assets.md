# Release assets

For developers: the files of a release and the `relsign` tool that signs them. How a release is
made and approved is in [../release-signing.md](../release-signing.md).

## The files of tag `vX.Y.Z`

Built by `.github/workflows/release.yml` on a pushed tag, signed and published after the
repository owner approves the publish job:

| File | Content |
|---|---|
| `restic-pve-linux-amd64` | the static binary |
| `VERSION` | the tag and a newline |
| `SHA256SUMS` | `<sha256>  <name>` lines for the binary and `VERSION` |
| `SHA256SUMS.sig` | base64 of the Ed25519 signature over the exact bytes of `SHA256SUMS` |

Only linux/amd64 is built. The install line and `restic-pve update` use
`https://github.com/norandom/restic-pve/releases/latest/download/<file>`.

## relsign

`cmd/relsign` creates the key and signs:

```
relsign genkey <private> <public>
relsign pubkey
relsign sign SHA256SUMS
relsign verify SHA256SUMS
relsign embedded <base64-pubkey>
```

The private key comes from the environment variable `RELEASE_SIGNING_KEY` and is never printed.
`verify` and `embedded` check against the public keys compiled into `relsign` from
`internal/relkeys/keys.go`, the same list the installed `restic-pve` trusts. `relsign` is built
by the release workflow for its own use and is not a release asset.

[Back to the index](../README.md)
