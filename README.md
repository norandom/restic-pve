# restic-pve

First time on a Proxmox VE host? Read [docs/onboarding.md](docs/onboarding.md).

`restic-pve` is one static Go binary with two roles. On a Proxmox VE host (host role) it lets a
backup machine pull a consistent image of a guest over SSH (`vzdump --stdout`) without root SSH
and without any other software on the hypervisor. On the backup machine (backup role) it turns
that image into a stream restic stores and deduplicates, turns a stored image back into a file
the host can restore, and shows the running backup. One static binary: its few libraries (the
terminal screens use `tcell` and `tview`) are compiled in and kept in this repository under
`vendor/`, so the build downloads nothing and neither machine needs shared libraries.

On the host there is the binary, one locked user with one restricted key, one sudoers file, one
settings file and a daily update timer. Everything the binary writes is compiled into it
(`go:embed`). `restic-pve uninstall` removes all of it.

This repository holds the tool, its tests, the release workflow and the releases. The backup
machine's control code and its configuration stay on that machine; `request` below is the
interface between the two machines, and `stream` is what the backup machine runs for each guest.

## Roles

The tool knows which machine it runs on:

- **Proxmox host**: `/etc/pve` is a folder or `/etc/restic-pve.conf` exists.
- **Backup machine**: the environment variable `RESTIC_PVE_STATE` is set (the state folder, for
  example `/state`).
- Both hold: Proxmox host. A Proxmox host never runs a backup-role command by accident.

A command of the other role is refused before it does anything, with exit 64 and one line, for
example `restic-pve install is for the Proxmox host; this is the backup machine. Nothing was
changed. Run it in a root shell on the Proxmox host.` When neither fact holds, the host
commands work as before, and a backup-role command says to set `RESTIC_PVE_STATE` (exit 78).
`restic-pve` without a command prints the usage of this machine's role.

`restic-pve version` prints the version on stdout exactly as before (`vX.Y.Z` and a newline;
released hosts compare it during their self-update). When stderr is a terminal it adds the role
on stderr, for example `role: backup machine`.

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

Backup machine:

| Command | Who | What |
|---|---|---|
| `stream backup\|verify\|to-vma\|to-raw` | the backup machine's control code | the stream program, see [Stream contract](#stream-contract) |
| `watch [--once]` | the admin | the block view of the guest backup that runs now; one plain line without a terminal or with `--once` |
| `version` | anyone | prints the version |

`watch` reads `<RESTIC_PVE_STATE>/guests/view` (the folder `stream backup --view-dir` writes)
and never writes. When no backup runs it shows `RESTIC_PVE_START_HINT`, the command that starts
one on this machine (for example `./backup.sh <source>`); without that variable it stops with
exit 78 and names it. It closes itself ten minutes after the last running backup; leaving it
never stops a backup. `NO_COLOR` keeps the layout without colours.

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

## Stream contract

`restic-pve stream` is the former `vmastream` program of the backup machine, moved here with its
contract unchanged. The control code that calls it parses this contract; any change to the
flags, the exit codes or the result line is a change of the interface.

A live VMA stream sends disk clusters in whatever order they arrive and interleaves several
devices, so two backups of an almost unchanged guest have little in common byte for byte.
`stream backup` reorders the clusters into plain per-device order (the RVS1 container): unchanged
disk regions give identical bytes, and restic deduplicates them. A full image is never stored on
disk; a bounded RAM reorder buffer handles out-of-order clusters.

| Command | Input | Output |
|---|---|---|
| `backup` | VMA via `ssh … dump <id>` or `--input FILE` | RVS1 on stdout, manifest JSON (`--manifest-out`) |
| `verify` | RVS1 on stdin or `--in` | fingerprint check against trailer and `--manifest` |
| `to-vma` | RVS1 on stdin or `--in` | uncompressed VMA (`--out FILE` or `-`) |
| `to-raw` | RVS1 on stdin or `--in` | `<dir>/<device>.raw` (sparse) and `<dir>/SHA256SUMS` |

`restic-pve stream <command> -h` lists the flags. All hosts, ids and paths come from flags; the
command has no built-in site value and no default folder.

**Environment.** restic starts `stream backup` with restic's own environment. Before anything
else, the command removes every variable that holds or points to a secret (names containing
`PASSWORD`, `PASSPHRASE`, `SECRET`, `TOKEN`, `WEBHOOK` or `CREDENTIAL`, or ending in `_KEY` or
`_PASS`, `RESTIC_PASSWORD` among them) from its own environment, so `ssh` and anything else it
starts never receive them. `VMASTREAM_VIEW=off` turns the block view off whatever the flags say.

### backup

`backup` runs `ssh -F <config> -T -o BatchMode=yes <host> dump <vmid>` (fixed argv, no shell;
`ssh` is looked up in `PATH`). The far side is `restic-pve request dump` on the Proxmox host,
which writes the VMA stream to stdout and the vzdump log to stderr. `--input FILE` reads a VMA
file instead (tests).

The pipeline is VMA reader (all structural checks) → reorder buffer → per-device `sha256ct`
fingerprint and RVS1 writer on stdout. The trailer is written last, only after the source
exited cleanly. The manifest (`--manifest-out`) is written only on success (temporary file,
then rename). Any failure ends the run before the trailer, so stdout never holds a complete
RVS1 stream for a failed backup. The trailer and the manifest name the producer
`vmastream dev`, as every stored image so far does, so the stored bytes stay the same.

Memory: `--ram-mib` (default 512) caps the reorder buffer: buffered clusters, patch hash slots
(32 bytes each) and the zero-cluster bitmaps (1 bit per 64 KiB cluster). It must cover the
bitmaps plus a 256 KiB margin. The RVS1 writer's open data frame (up to 64 MiB) and the Go
runtime come on top: plan about `ram-mib + 64 MiB + overhead`. When the cap would be exceeded,
the farthest early cluster is written ahead as a patch and only its hash stays in RAM. Only
when hash slots and bitmaps exhaust the cap does the backup stop with exit 7: raise
`--ram-mib`, or lower `MAX_WORKERS` in `/etc/restic-pve.conf` on the host.

`--view-dir DIR` writes the block view's state file (`live`, `vm-<vmid>.last`) for `watch`;
best effort, it never affects the stream. Without it there is no view. `--view-name` is the
guest name shown.

vzdump log lines are classified by one pattern table:

| Log | Result |
|-----|--------|
| `issuing guest-agent 'fs-freeze'`, no freeze error | consistency `application` |
| `skipping guest-agent 'fs-freeze'` | consistency `crash` + detail |
| fs-freeze error or timeout | consistency `crash` + detail |
| freeze failure with `Input/output error`, `I/O error`, `Structure needs cleaning`, or `Read-only file system` | ssh is stopped at once, exit 10; check the guest (fsck) |
| `backup mode: stop` (a guest that is not running), no freeze error | consistency `stopped` |
| no freeze line, or an unrecognised log | consistency `crash`, detail `agent status unknown` |
| any fleecing warning or error | ssh is stopped, exit 5 |
| qemu-img `Formatting '…'`, PVE notification failures | info; never forwarded |

Lines without a vzdump level prefix come from ssh or the host side and count as errors, except
the known informational lines above. Host exit 73 (fleecing storage below its free-space limit)
is a non-transient source failure (exit 6).

```sh
restic-pve stream backup --ssh-config ./ssh_config --ssh-host pve.example --vmid 100 \
  --manifest-out ./vm-100.manifest.json > vm-100.rvs
```

### verify, to-vma, to-raw

All three read RVS1 in one pass from stdin, or from `--in FILE`. They recompute `sha256ct` for
every device and compare it with the trailer and, with `--manifest M`, with the manifest
(devices, sizes, digests). A digest mismatch or a malformed stream exits 3, a manifest mismatch
exits 8, a manifest file that cannot be opened exits 7. Each patched position must be patched
only once, and its placeholder must be zero.

- `verify` writes nothing but the stderr result.
- `to-vma` writes a VMA stream with a fresh uuid, the current time, the config blobs verbatim
  and every cluster of every device exactly once. The final extent is written only after every
  digest matched; on any failure the output is a truncated VMA stream that `qmrestore` rejects.
  `--out FILE` must not exist.
- `to-raw --dir D` writes `D/<device>.raw` per device as sparse files and then `D/SHA256SUMS`
  (`sha256sum -c` checks it). Existing files are never overwritten (exit 7). On any failure every
  raw file is renamed to `*.raw.invalid` and no `SHA256SUMS` is written.

```sh
restic-pve stream verify --manifest ./vm-100.manifest.json < vm-100.rvs
restic-pve stream to-vma --in vm-100.rvs --manifest ./vm-100.manifest.json --out vm-100.vma
restic-pve stream to-raw --in vm-100.rvs --dir ./restore/example
```

### Exit codes and stderr

| Code | Meaning |
|------|---------|
| 0 | ok |
| 2 | usage (unknown command, bad flags or arguments) |
| 3 | integrity (VMA md5/uuid/reserved/blockinfo error, digest mismatch, malformed RVS1) |
| 4 | forbidden content (memory state or unknown device) |
| 5 | fleecing not active (reported by vzdump) |
| 6 | source failed (ssh/vzdump non-zero exit, truncated stream) |
| 7 | local I/O (input/output files, stdout, output folder), RAM cap exhausted |
| 8 | manifest mismatch or missing device |
| 9 | internal error (panic or contract violation; never transient) |
| 10 | guest filesystem error during freeze (immediate stop; never transient) |

The role guard comes first: on a Proxmox host `stream` is refused with exit 64 and no result
line, and without `RESTIC_PVE_STATE` with exit 78.

`transient` is true for ssh exit 255 (connection, authentication or host key), host exit 75
(busy) and an ssh killed by a foreign signal. An explicit `ERROR: Backup of VM <id> failed -
<reason>` overrides the ssh status: only reasons matching `lock`, `locked`, `busy`, `timeout`,
`timed out`, `temporarily`, `try again` or `connection` are transient. Exit 10 reports
`guest filesystem error: <the vzdump line>` with the hint `check the guest (fsck); the backup was
not written`.

Stdout carries stream data only. Stderr carries human-readable messages and JSON lines (one
object per line, no prefix). The last line is always the result:

```json
{"type":"result","status":"ok|error","code":0,"transient":false,"message":"…"}
```

`help` and `-h` print usage and exit 0 without a result line. A caller treats a missing result
line as a non-transient failure. `backup` also writes:

```json
{"type":"progress","dev":"drive-scsi0","bytes_done":0,"total_bytes":0,"percent":0,"ram_bytes":0,"patches":0}
{"type":"consistency","value":"application|crash|stopped","detail":"…"}
{"type":"vzdump","level":"warn|error","line":"…"}
```

`progress` comes every `--progress-interval` and once at the end. A successful backup result
also has `buffer_ram_peak` and `patch_count`. `consistency` comes once. `vzdump` forwards the
warn and error lines of the vzdump log only.

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

Size of the release binary (linux/amd64, `golang:1.27.1-alpine3.24`, the workflow's flags,
static, measured 2026-11-19): 8,544,380 bytes (8.1 MiB) with both roles; the host role alone
was 7,471,228 bytes (7.1 MiB). The gate is 16 MiB; the self-update accepts at most 64 MiB.

The wrapper tests run the test binary as the wrapper (helper process) against
stub `qm`, `pvesm`, `pveversion` and `vzdump` scripts in a temp directory. They
need no PVE host and no root.
