# restic-pve on the Proxmox host

For an admin who wants to know exactly what `restic-pve` puts on a Proxmox VE host, what the
backup user can do there, how updates are checked, and how to remove it all.

## What ends up on the host

| Path | What it is |
|---|---|
| `/usr/local/sbin/restic-pve` | the program, one static binary |
| user `restic-backup` | password locked, shell `/bin/sh`; it logs in only with the key below |
| `/home/restic-backup/.ssh/authorized_keys` | one key entry, accepted only from the backup machine's address |
| `/etc/sudoers.d/restic-pve` | lets that user run `restic-pve request` as root, nothing else |
| `/etc/restic-pve.conf` | the settings: fleecing storage, limits, allowed guests |
| `/etc/systemd/system/restic-pve-update.service` and `.timer` | the daily self-update |
| `/run/restic-pve.lock` | held while a guest image is being sent |

With changed reads (install, answer yes to "Changed reads"; off unless you say yes), two more
items, and one while a backup runs:

| Item | What it is | Removed by |
|---|---|---|
| `/usr/share/perl5/PVE/Storage/Custom/ResticPVEPlugin.pm` | the Proxmox backup provider plug-in, written by the binary from its own copy; its first line says so | install with no, `restic-pve uninstall` |
| storage `restic-pve` in `/etc/pve/storage.cfg` | added with `pvesm add`; the Proxmox screens show it as a backup target that refuses jobs started there | install with no, `restic-pve uninstall` |
| `/run/restic-pve/provider.sock` | the socket of a running backup, root only | the backup when it ends; the next backup or a reboot after a crash |

Proxmox gives its record of changed blocks only to such a plug-in; with it, a backup of a
running guest reads only the parts that changed since the last one. Adding or removing the file
reloads `pvestatd`, `pvedaemon` and `pveproxy` once (`systemctl reload-or-try-restart`, as
Proxmox's own packages do; open sessions and running tasks go on). A file at that path that
restic-pve did not write is never touched. Install will not change the plug-in while a backup
runs.

Before it writes anything, install checks and refuses "yes", with nothing changed, when:

- the host is part of a Proxmox cluster (`/etc/pve/corosync.conf` exists): the storage entry
  would be cluster-wide, the plug-in only on this node. Changed reads work on single hosts for
  now;
- this Proxmox has no backup provider interface (before Proxmox VE 8.4);
- this Proxmox no longer accepts the plug-in of the installed restic-pve: run
  `restic-pve update` first.

After writing the file it checks that Proxmox loads it, then adds the entry; if either fails,
the file is put back as it was and the message says what failed. A Proxmox newer than the
plug-in was checked against still loads it, but prints `Plugin "PVE::Storage::Custom::ResticPVEPlugin"
is implementing an older storage API, an upgrade is recommended` in its commands, also on the
log of an ordinary `dump`; install, the update and `request info` say so, and
`restic-pve update` fixes it.

### What Proxmox's own "backup OK" means for the storage restic-pve

Every job to the storage `restic-pve` shows in Proxmox's task log as a successful backup when
it ends well, also when nothing was stored: a probe only looks at the record, and an "arm" job
only starts it. Proxmox's success is the backup machine's confirmation of that one job, not a
proof that anything was stored; it never lists a backup on that storage either. The task log
gets one line that says what happened: `restic-pve: probe: nothing read or stored`,
`restic-pve: armed: change record started, nothing stored`, or `restic-pve: stored: confirmed by
the backup machine as snapshot <id>`. The proof of a stored backup is the backup machine's
`restic-pve status`. What the owner of the host can rely on: the backup identity cannot run a
command, reach a file or another guest, or change the host's configuration through these jobs.

A job to `restic-pve` is an ordinary vzdump run: a site hook script set in
`/etc/vzdump.conf` runs for it as for `dump`. Settings there that would act after the backup
machine stored and confirmed the job are switched off for it (`--protected 0 --prune-backups
keep-all=1 --remove 0`), and a notes template does nothing on this storage.

If the `sudo` package is missing, install adds it, and uninstall leaves it. Every file install
writes is compiled into the binary; nothing is downloaded during install. Install also removes
the files of the older bash wrapper if it finds them: `/usr/local/sbin/restic-vzdump`,
`/etc/sudoers.d/restic-vzdump`, `/etc/restic-vzdump.conf` and `/etc/restic-vzdump.allow`.

Install is idempotent and reports each file as `written` or `unchanged`. A new guest or a changed
limit is the same `restic-pve install` again.

## What the backup user can and cannot do

The key entry and the sudo rule look like this (the address and key are examples):

```
restrict,from="192.0.2.10",command="sudo -n /usr/local/sbin/restic-pve request $SSH_ORIGINAL_COMMAND" ssh-ed25519 AAAA...
restic-backup ALL=(root) NOPASSWD: /usr/local/sbin/restic-pve request, /usr/local/sbin/restic-pve request *
```

Whatever command the backup machine sends, SSH runs `restic-pve request` with it, and `restrict`
turns off port forwarding, agent forwarding and terminals. `request` answers three things:

- `ping` prints `ok`;
- `info <vmid>` prints one JSON line about an allowed guest (disks, fleecing state, versions);
- `dump <vmid>` sends the image of an allowed guest, made by `vzdump --stdout` with fleecing;
- `changes <vmid> probe|use|new` (only with changed reads on) starts one backup of an allowed
  guest to the storage `restic-pve`, with fleecing and the read limit, and sends what changed:
  `probe` only the map of changed parts, `use` the changed parts too, `new` nothing (it starts
  Proxmox's record).

Anything else is refused with exit 64, and so is a guest that is not in `VMIDS`. With
`changes` the backup user still cannot name a storage, a path or a vzdump option, cannot run two
backups at once, and cannot reach the plug-in's socket: only the vzdump that request started
may connect to it. Proxmox counts a job as done only when the backup machine confirmed that
job (see above for what that means). A job holds the host for a bounded time: when the backup
machine reads nothing of the stream for 10 minutes, or sends no confirmation within an hour
after it, the job ends and Proxmox counts it failed. Without changed reads, or on a cluster
node, `changes` is refused with exit 79 and says what to do. An empty
command is refused too, so the user has no login shell in practice. The details are in
[reference/request-interface.md](reference/request-interface.md).

## The settings file

`/etc/restic-pve.conf`, written by install:

```
FLEECING_STORAGE=local
FLEECING_MIN_FREE_GIB=32
FLEECING_ABORT_FREE_GIB=8
BWLIMIT_KIB=40960
MAX_WORKERS=1
VMIDS=100 101
```

| Key | Meaning |
|---|---|
| `FLEECING_STORAGE` | where vzdump keeps temporary data while a running guest is read |
| `FLEECING_MIN_FREE_GIB` | a dump starts only with this much free on that storage |
| `FLEECING_ABORT_FREE_GIB` | a running dump is stopped below this; must be below the first |
| `BWLIMIT_KIB` | read limit in KiB/s (the form asks in MiB/s; 40 MiB/s is 40960) |
| `MAX_WORKERS` | vzdump's `max-workers`, 1 to 16 |
| `VMIDS` | the allowed guests, separated by single spaces |

Change it with `restic-pve install`, not by hand. Every key must appear exactly once; a missing
or broken file makes every `info` and `dump` request fail with exit 78.

## Updates

`restic-pve-update.timer` runs `restic-pve update` once a day, at a random time within six hours
of midnight, and catches up after a reboot. It is always on; there is no setting for it. To
update now, as root:

```sh
restic-pve update
```

The update downloads `SHA256SUMS` and its signature from the latest release, checks the
signature against the keys built into the installed binary, checks the hashes of `VERSION` and
the new binary, and accepts only a strictly newer version. It runs the new binary with `version`
and then renames it over the old one, so a dump that is running finishes with the old file.
With changed reads on, the new binary then brings the plug-in file to its own copy, unless a
backup runs; then the next backup does it before it starts.
Any failure leaves the installed binary as it was and exits 1; then
`systemctl status restic-pve-update.service` shows the reason.

### Check a download

The first download is protected by HTTPS only. Before `restic-pve install`, compare it with the
release checksums:

```sh
curl -fsSL https://github.com/norandom/restic-pve/releases/latest/download/SHA256SUMS
sha256sum /usr/local/sbin/restic-pve      # must match the restic-pve-linux-amd64 line
```

This shows that the file arrived intact. To check that `SHA256SUMS` itself is signed, see
[release-signing.md](release-signing.md#check-a-download-by-hand).

## Remove it

As root:

```sh
restic-pve uninstall
```

It first removes changed reads (the storage `restic-pve`, the plug-in file and its folder when
empty, `/run/restic-pve/`). If `pvesm remove` fails, the plug-in file stays with its entry,
everything else is still removed except the binary, and the last line says what is left and the
command to finish (exit 1). Then stops and removes the update timer and service, the sudoers
file, the settings file, the old wrapper files, the lock file, the backup user with its home,
and last the binary.
Each file is reported as `removed` or `absent`. It refuses with exit 75 while a dump is running.
It removes the user only when the sudoers file is the one install wrote; otherwise it leaves
users alone and says so.

[Back to the index](README.md)
