# restic-pve

First time on a Proxmox VE host? Read [docs/onboarding.md](docs/onboarding.md).

`restic-pve` is one static Go binary for a Proxmox VE host. It lets a backup machine pull a
consistent image of a guest over SSH (`vzdump --stdout`) without root SSH and without any other
software on the hypervisor. One static binary: its few libraries (the terminal screens use
`tcell` and `tview`) are compiled in and kept in this repository under `vendor/`, so the build
downloads nothing and the host needs no shared libraries.

On the host there is the binary, one locked user with one restricted key, one sudoers file, one
settings file and a daily update timer. Everything the binary writes is compiled into it
(`go:embed`). `restic-pve uninstall` removes all of it.

This repository holds the tool, its tests, the release workflow and the releases. The backup
host side, which stores the stream, is a separate private project; `request` below is its
interface.

## Install, update, remove

In a root shell on the Proxmox VE host:

```
curl -fsSLo /usr/local/sbin/restic-pve https://github.com/norandom/restic-pve/releases/latest/download/restic-pve-linux-amd64 && chmod 755 /usr/local/sbin/restic-pve && restic-pve install
```

`install` opens a full-screen form in the terminal (a retro disk-tool look, see
[docs/house-style.md](docs/house-style.md)), one screen per value: the backup machine address, its
public key, the guests (a checklist), the storage for fleecing images (a list), the limits and a
summary with `[ Install ]`. Nothing changes before Install. Run `restic-pve install` again to
change a value. Without a terminal, or with `restic-pve install --plain`, it asks the same
questions line by line.

The host then updates itself: `restic-pve-update.timer` runs `restic-pve update` daily, which
accepts only a newer release whose checksums are signed with a key built into the installed
binary. `restic-pve update` does the same by hand. `restic-pve uninstall` removes everything.

The first download is protected by HTTPS only; [docs/release-signing.md](docs/release-signing.md)
shows how to check it against the signed checksums.

## Subcommands

| Command | Who | What |
|---|---|---|
| `request ping` | backup identity (forced command, sudo) | prints `ok` |
| `request info <vmid>` | backup identity | one JSON line: guest, disks, fleecing state, conf values, PVE and binary version |
| `request dump <vmid>` | backup identity | `vzdump --stdout` VMA stream on stdout, vzdump log on stderr |
| `install [--plain]` | root | asks for the site values (form, or line by line), installs or changes the identity |
| `update` | root | replaces itself with the latest signed release |
| `uninstall` | root | removes the identity, its files and the binary |
| `version` | anyone | prints the version |

`request` exit codes:
0 ok, 2 VM missing, 64 refused, 73 fleecing storage below its free-space limit,
75 busy, 78 invalid `/etc` configuration, otherwise vzdump's status. It reads one
settings file, `/etc/restic-pve.conf` (strict `KEY=value`, every key once), and
locks `/run/restic-pve.lock` during a dump. Every other request is refused with
exit 64.

```
FLEECING_STORAGE=fast-ssd
FLEECING_MIN_FREE_GIB=32
FLEECING_ABORT_FREE_GIB=8
BWLIMIT_KIB=40960
MAX_WORKERS=1
VMIDS=100 101
```

`VMIDS` is the allow list: guest IDs separated by single spaces, no duplicates.
A missing or invalid file is exit 78; a guest not in `VMIDS` is refused (64).

The backup key and sudo reach only `request`:

```
restrict,from="192.0.2.10",command="sudo -n /usr/local/sbin/restic-pve request $SSH_ORIGINAL_COMMAND" ssh-ed25519 AAAA...
restic-backup ALL=(root) NOPASSWD: /usr/local/sbin/restic-pve request, /usr/local/sbin/restic-pve request *
```

The first sudoers entry matches `request` alone, so an empty ssh command reaches
the binary and is refused with exit 64.

### install

`install` takes no argument or `--plain` (anything else is exit 64). It runs as root on a PVE
host only: when `qm` or `pvesm` is not in `/usr/sbin:/usr/bin:/sbin:/bin` it stops with one line.
The values and their rules:

1. backup machine address for `from=` (`^[A-Za-z0-9.:,*?!/_-]{1,255}$`)
2. public key, one line `ssh-ed25519 <base64> [comment]` (a real ed25519 key blob; only type and
   key are stored)
3. guests, each once, each listed by `qm list`, at least one
4. fleecing storage, one of the active storages of `pvesm status`
5. the limits: `FLEECING_MIN_FREE_GIB`, `FLEECING_ABORT_FREE_GIB` (below the first), the read
   limit in MiB/s (stored as `BWLIMIT_KIB`), `MAX_WORKERS` (1-16)

**Form.** When stdin and stdout are a terminal, `TERM` is set and not `dumb`, the terminal is at
least 60x20 and `--plain` is not given, install shows a full-screen form in the project's retro
disk-tool look ([docs/house-style.md](docs/house-style.md)): a blue screen, a grey title bar with
the step, a framed box with the explanation and the field or list, a Progress box and a Help box
below it, and a grey key bar at the bottom. The terminal is restored on exit, Ctrl-C, SIGTERM,
SIGHUP and panic. One screen per value: text fields for address and key (an empty field shows a
grey example that is never taken as the value; the Help box shows the example too), a checklist
of `qm list` (Up/Down or j/k, Space ticks), a single choice of the active storages with free GiB,
the limits with "Keep these (recommended)" or "Change" (four number fields, the Help box shows
the range of the field being edited), and a summary with `[ Install ]  [ Back ]  [ Cancel ]`,
focus on Install. The focused row or button is yellow. Enter continues when the value is valid,
else its rule shows in the Help box and the screen stays; Esc or Shift-Tab goes one screen back
with the values kept; Ctrl-C or Cancel is exit 1 and nothing changed. A pasted key with a
trailing newline counts as Enter; bracketed paste is understood. `NO_COLOR` keeps the layout
without colours (focus in inverse video); a locale that is not UTF-8 gets ASCII frames.

**Plain** (`--plain`, or no terminal). One answer per line from stdin, the questions on stdout,
with an example for address and key. Enter takes the default in brackets; an invalid answer
prints its rule and the question comes again; three invalid answers, or stdin at its end
(`no terminal to ask on`), stop with exit 1 and nothing changed. The guests are typed as VM
numbers separated by spaces or commas; the limits follow `Change the limits? [y/N]`.

The defaults on a fresh host: no address, no key, no guests, no storage, limits 32 / 8 GiB,
40 MiB/s, 1 worker. On an installed host they are the current values: address and key from the
backup user's `authorized_keys`, the rest from `/etc/restic-pve.conf`. The user is
`restic-backup`; a different user in an existing `/etc/sudoers.d/restic-pve` (older install) is
kept. A summary follows; in plain mode then `Install with these values? [y/N]`, where anything
but `y` is exit 1 and nothing changed.

Install is idempotent and reports each file as `written` or `unchanged`: the sudo package
(only if missing), the user (shell `/bin/sh`, password locked), `~/.ssh/authorized_keys`,
`/etc/sudoers.d/restic-pve` (checked with `visudo -cf` first), the binary at
`/usr/local/sbin/restic-pve`, `/etc/restic-pve.conf` and the automatic update:
`/etc/systemd/system/restic-pve-update.service` (oneshot, `restic-pve update`) and
`/etc/systemd/system/restic-pve-update.timer` (daily, random delay up to 6 h, persistent), both
compiled in from `internal/install/units/`. It then runs `systemctl daemon-reload` and
`systemctl enable --now restic-pve-update.timer`. The timer is always on; there is no setting
for it. It then removes the files of the bash
wrapper if present: `/usr/local/sbin/restic-vzdump`, `/etc/sudoers.d/restic-vzdump`,
`/etc/restic-vzdump.conf` and `/etc/restic-vzdump.allow`. A new guest or a changed limit is the
same install again.

`install` reads the answers or keys from its own stdin, the terminal. On the PVE host the binary is
downloaded straight to `/usr/local/sbin/restic-pve` and run from there; install then leaves the file in place and only sets owner and mode. Run
from any other path, it copies itself there.

### uninstall

`uninstall` refuses with exit 75 while a dump holds the lock. Otherwise it runs
`systemctl disable --now restic-pve-update.timer` (if the timer file is there) and removes,
each reported as `removed` or `absent`: both update units (then `systemctl daemon-reload`),
`/etc/sudoers.d/restic-pve`,
`/etc/restic-pve.conf`, the four bash wrapper files, the lock file, the backup user
with its home (`userdel --remove`; the name comes from the sudoers entry, and when
that entry is missing or not the one install writes, users are left alone), and
last the binary. The sudo package stays.

### update

`update` downloads from `https://github.com/<repo>/releases/latest/download/`,
verifies `SHA256SUMS.sig` against the Ed25519 keys compiled into the binary
(`internal/relkeys`), checks the hashes of `VERSION` and the binary, requires a
strictly newer version, runs the new binary with `version`, and renames it over
the installed one. Any failure leaves the installed binary untouched and exits 1,
so the timer's service shows as failed. Up to date is one line and exit 0. A build
without keys refuses to update. The rename is atomic: a dump that is running keeps
the old file open and finishes with it.

## Release assets (tag `vX.Y.Z`)

Built by `.github/workflows/release.yml` on a pushed tag, signed and published after the
repository owner approves the publish job (see [docs/release-signing.md](docs/release-signing.md)):
`restic-pve-linux-amd64`, `VERSION` (the tag plus newline), `SHA256SUMS` (`<sha256>  <name>`
lines for the binary and `VERSION`) and `SHA256SUMS.sig` (base64 of the Ed25519 signature over
the exact bytes of `SHA256SUMS`).

`cmd/relsign` creates the key and signs: `relsign genkey <private> <public>`,
`relsign pubkey`, `relsign sign SHA256SUMS`, `relsign verify SHA256SUMS`,
`relsign embedded <base64-pubkey>`. The private key comes from the environment
variable `RELEASE_SIGNING_KEY` and is never printed.

## Build and test

Go runs in the pinned image only:

```
docker run --rm -u "$(id -u):$(id -g)" -e CGO_ENABLED=0 -e GOPROXY=off -e GOTOOLCHAIN=local \
  -e GOCACHE=/tmp/gocache -e GOPATH=/tmp/gopath -v "$PWD:/src:ro" -w /src golang:1.27.1-alpine3.24 \
  sh -c 'gofmt -l cmd internal retro blockview ; go vet ./... && go test ./...'
```

The libraries are vendored (`vendor/`, pinned in `go.mod` and `go.sum`); `GOPROXY=off` proves
that nothing is fetched. To update a library, run `go get <module>@<version> && go mod tidy &&
go mod vendor` once in the same image with the network, and review the `vendor/` diff.

Release build (the release workflow does this):

```
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=vX.Y.Z -X main.releaseRepo=<owner>/<repo>" \
  -o restic-pve-linux-amd64 ./cmd/restic-pve
```

The wrapper tests run the test binary as the wrapper (helper process) against
stub `qm`, `pvesm`, `pveversion` and `vzdump` scripts in a temp directory. They
need no PVE host and no root.
