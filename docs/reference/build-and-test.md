# Build and test

For developers: how to build and test `restic-pve` the same way the release workflow does, how the
libraries are vendored, and how big the binary is.

## Checks

Go runs in the pinned image only. From the repository root:

```
docker run --rm -u "$(id -u):$(id -g)" -e CGO_ENABLED=0 -e GOPROXY=off -e GOTOOLCHAIN=local \
  -e GOCACHE=/tmp/gocache -e GOPATH=/tmp/gopath -v "$PWD:/src:ro" -w /src golang:1.27.1-alpine3.24 \
  sh -c 'gofmt -l cmd internal retro blockview ; go vet ./... && go test ./...'
```

`gofmt -l` must print nothing. The release workflow runs the same three checks, with the image
also pinned by digest (`GO_IMAGE` in `.github/workflows/release.yml`), and fails on any of them.

## Libraries

The libraries (the terminal screens use `tcell` and `tview`) are vendored in `vendor/` and
pinned in `go.mod` and `go.sum`. `GOPROXY=off` shows that nothing is fetched. To update a
library, run `go get <module>@<version> && go mod tidy && go mod vendor` once in the same image
with the network, and review the `vendor/` diff.

## Release build

The release workflow builds with:

```
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -buildid= -X main.version=vX.Y.Z -X main.releaseRepo=<owner>/<repo>" \
  -o restic-pve-linux-amd64 ./cmd/restic-pve
```

Without `-X main.version` the binary reports `dev`. `CGO_ENABLED=0` makes it static; it needs no
shared library on either machine.

## Size

Release binary, linux/amd64, `golang:1.27.1-alpine3.24`, the workflow's flags, measured
2026-11-20 with changed reads on the host side (plug-in management, job server, NBD client,
the `changes` request) and the probe of `pieces`: 9,449,596 bytes (9.0 MiB). Before them, the
same day: 9,134,204 bytes (8.7 MiB) with both roles and the status, keys and space screens. On
2026-11-19 it was
9,072,764 bytes; before the screens 8,544,380 bytes, and the host role alone 7,471,228 bytes
(7.1 MiB). The project's limit is 16 MiB; no workflow step checks it, so measure after a change
that adds a library. `restic-pve update` refuses a binary larger than 64 MiB.

## Tests without Proxmox

No test needs a Proxmox host or root:

- The `request` tests run the test binary as the wrapper (a helper process) against stub `qm`,
  `pvesm`, `pveversion` and `vzdump` scripts in a temporary directory.
- The stream tests build VMA streams in memory and check the round trip through RVS1, `verify`,
  `to-vma` and `to-raw`.
- The screen tests draw on tcell's simulation screen and compare the text.

- The NBD client tests run against a fake NBD server in Go (`internal/nbd/nbdtest`), which
  serves an export with a dirty bitmap and can break the protocol in a dozen ways.
  `internal/nbd/nbdtest/fakenbd` is the same server as a program, for trials without Proxmox; it
  is never part of a release.
- The `pieces` tests use the test binary as `ssh` and the Proxmox side behind it: for the
  stored runs a fake host with two disk images, Proxmox's change record of each disk, the
  changes stream and the ack, and `dump` as a VMA stream; against restic 0.19.1 they take
  about five minutes.

## Tests that need restic or Perl

A few tests need programs the Go image does not have: the restic release the backup image pins
(0.19.1), and Perl for the Proxmox plug-in. They find them as follows, and skip with a message
that says how to provide them when they are missing. So `go test ./...` passes offline in the
release workflow, where these tests are skipped; nothing is downloaded by a test or by the
workflow.

| Variable | Default | Used by |
|---|---|---|
| `RESTIC_PVE_TEST_RESTIC` | `<user cache dir>/restic-pve-test/restic-0.19.1` | `internal/pieces`, `internal/testfix`, `internal/stream/cli` (a pieces snapshot through `verify`, `to-vma`, `to-raw --pieces`) |
| `RESTIC_PVE_TEST_PERL` | `perl` in `PATH` | `internal/provider` |
| `RESTIC_PVE_TEST_REQUIRE` | unset | `1` turns every such skip into a failure |

A restic that is not the pinned binary fails the test (SHA-256
`20d4142678d0d95ec11a4759def1b73fd9190abc9ca19e4b62d067c0b387e639` and `restic version`), and
a Perl set by the variable that lacks `IO::Socket::UNIX`, `JSON::PP` or `MIME::Base64` fails
too. Debian's slim images have only `perl-base`, without `JSON::PP`; use the official Perl image.

Get the restic binary once, into a folder of your choice (`fix` below), either from the
release with its checksum:

```
mkdir -p fix && cd fix
curl -fsSLO https://github.com/restic/restic/releases/download/v0.19.1/restic_0.19.1_linux_amd64.bz2
echo "f415415624dcc452f2a02b8c33641791a8c6d6d3b65bbb3543fcf9a25151585c  restic_0.19.1_linux_amd64.bz2" | sha256sum -c -
bunzip2 -c restic_0.19.1_linux_amd64.bz2 > restic-0.19.1 && chmod 755 restic-0.19.1 && cd ..
```

or out of a built backup image: `docker run --rm --entrypoint cat <backup image>
/usr/local/bin/restic > fix/restic-0.19.1`. Then, from the repository root:

```
docker run --rm -u "$(id -u):$(id -g)" -e CGO_ENABLED=0 -e GOPROXY=off -e GOTOOLCHAIN=local \
  -e GOCACHE=/tmp/gocache -e GOPATH=/tmp/gopath -e RESTIC_PVE_TEST_RESTIC=/fix/restic-0.19.1 \
  -v "$PWD:/src:ro" -v "$PWD/fix:/fix" -w /src golang:1.27.1-alpine3.24 sh -c 'go test -count=1 ./... &&
  RESTIC_PVE_TEST_REQUIRE=1 go test -count=1 ./internal/pieces ./internal/testfix ./internal/stream/cli &&
  go test -c -o /fix/provider.test ./internal/provider && go test -c -o /fix/wrapper.test ./internal/wrapper'
for p in provider wrapper; do docker run --rm -u "$(id -u):$(id -g)" -e RESTIC_PVE_TEST_REQUIRE=1 \
  -v "$PWD:/src:ro" -v "$PWD/fix:/fix:ro" -w /src/internal/$p perl:5.40-slim /fix/$p.test -test.count=1 -test.v; done
```

The release workflow runs only the first part (`go test ./...` in the Go image, the Perl and
restic tests skipped). The Perl-dependent tests are a required manual gate before a tag: run
both commands above with `RESTIC_PVE_TEST_REQUIRE=1` and a clean result.

The first command runs every test (the Perl ones skip: the Go image has no Perl), then the
restic tests again with skips turned into failures, and builds the plug-in and the request tests
as static test binaries. The second runs them next to Perl 5.40 (the request tests run the
`changes` request end to end: the real plug-in loaded by the Perl stand-in of vzdump, a fake NBD
export, the job server), the Perl of Debian 13 on which
Proxmox VE 9 is built, also with skips turned into failures. What the plug-in tests cannot
show is in
[provider-plugin.md](provider-plugin.md#tests-without-proxmox).

[Back to the index](../README.md)
