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
2026-11-19: 9,072,764 bytes (8.7 MiB) with both roles and the status, keys and space screens.
Before those screens it was 8,544,380 bytes, and the host role alone was 7,471,228 bytes
(7.1 MiB). The project's limit is 16 MiB; no workflow step checks it, so measure after a change
that adds a library. `restic-pve update` refuses a binary larger than 64 MiB.

## Tests without Proxmox

No test needs a Proxmox host or root:

- The `request` tests run the test binary as the wrapper (a helper process) against stub `qm`,
  `pvesm`, `pveversion` and `vzdump` scripts in a temporary directory.
- The stream tests build VMA streams in memory and check the round trip through RVS1, `verify`,
  `to-vma` and `to-raw`.
- The screen tests draw on tcell's simulation screen and compare the text.

[Back to the index](../README.md)
