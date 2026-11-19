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
- `dump <vmid>` sends the image of an allowed guest, made by `vzdump --stdout` with fleecing.

Anything else is refused with exit 64, and so is a guest that is not in `VMIDS`. An empty
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

It stops and removes the update timer and service, then removes the sudoers file, the settings
file, the old wrapper files, the lock file, the backup user with its home, and last the binary.
Each file is reported as `removed` or `absent`. It refuses with exit 75 while a dump is running.
It removes the user only when the sudoers file is the one install wrote; otherwise it leaves
users alone and says so.

[Back to the index](README.md)
