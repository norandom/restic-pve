# Releases and signing

A release is built by GitHub Actions and published only after the repository owner approves it.
`restic-pve update` accepts a release only when its checksums are signed with an Ed25519 key that
is built into the installed binary. The private key exists in two places: the GitHub environment
`release` of this repository and the owner's password manager.

## What a release contains

`restic-pve-linux-amd64`, `VERSION` (the tag), `SHA256SUMS` (SHA-256 of the binary and of
`VERSION`) and `SHA256SUMS.sig` (Ed25519 signature over `SHA256SUMS`).

## How a release is made

1. Push a tag: `git tag v1.2.3 && git push origin v1.2.3`.
2. Job `build` (no secret): gofmt of `cmd` and `internal`, `go vet`, `go test`, builds the binary from
   the vendored sources in the pinned Go image,
   writes `VERSION` and `SHA256SUMS`.
3. Job `publish` waits for the approval of the required reviewer of the environment `release`.
4. After the approval it checks `SHA256SUMS`, refuses to sign when the signing key is not built
   into this release (`relsign embedded`), signs, verifies the signature and publishes the
   release.

A manual run (Actions, release, Run workflow) is a dry run: build and artifacts only, nothing is
signed or published.

## One-time setup (repository owner)

Create the key pair, on a machine with Go or in the pinned Go image:

```sh
go run ./cmd/relsign genkey ~/restic-release-key/release-signing.key ~/restic-release-key/release-signing.pub
```

Add the content of the `.pub` file as one quoted line between `// BEGIN KEYS` and `// END KEYS`
in `internal/relkeys/keys.go` and commit it. Then create the environment with yourself as
required reviewer and store the private key as its secret. The key is read from the file and is
never shown:

```sh
printf '{"reviewers":[{"type":"User","id":%s}]}' "$(gh api user --jq .id)" |
  gh api -X PUT repos/norandom/restic-pve/environments/release --input -
gh secret set RELEASE_SIGNING_KEY --env release --repo norandom/restic-pve < ~/restic-release-key/release-signing.key
```

Keep a copy of the private key in the password manager.

## How update verifies

`restic-pve update` (by hand, or daily from `restic-pve-update.timer`) downloads `SHA256SUMS` and
`SHA256SUMS.sig` from the latest release and checks the signature against the keys built into the
installed binary. It then checks the SHA-256 of `VERSION` and of the binary, accepts only a
strictly newer version, runs the new binary with `version`, and renames it over the installed
one. Any failure leaves the installed binary untouched and exits 1.

The first install is different: the install line downloads the binary over HTTPS from GitHub and
runs it, so that download is protected by HTTPS only.

## Check a download by hand

On the Proxmox VE host, before `restic-pve install`:

```sh
curl -fsSL https://github.com/norandom/restic-pve/releases/latest/download/SHA256SUMS
sha256sum /usr/local/sbin/restic-pve      # must match the restic-pve-linux-amd64 line
```

On your own machine, check that `SHA256SUMS` is signed. `relsign` verifies against the keys built
into it, so build it from the tag you check:

```sh
git checkout v1.2.3
go build -o /tmp/relsign ./cmd/relsign
mkdir /tmp/rel && cd /tmp/rel
gh release download v1.2.3 --repo norandom/restic-pve
/tmp/relsign verify SHA256SUMS      # SHA256SUMS: signature ok
sha256sum -c SHA256SUMS             # every asset OK
```

## A lost or leaked key

The installed binary trusts only the keys built into it.

- Planned rotation: publish one release that is signed with the old key and carries both keys,
  let the hosts update, then sign with the new key and remove the old line from `keys.go`.
- No trusted key left: `restic-pve update` refuses every new release. Run the install line on the
  host again; it replaces the binary without the signature check.
