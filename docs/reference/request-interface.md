# The request interface

For developers: `restic-pve request`, the only thing the backup machine can call on the Proxmox
host. It covers the commands, the settings file, the SSH and sudo entries, and the exit codes.

## Commands

The backup machine logs in as the backup user; the forced command passes whatever it sent to
`sudo -n /usr/local/sbin/restic-pve request`.

| Request | Output |
|---|---|
| `ping` | `ok` on stdout |
| `info <vmid>` | one JSON line on stdout: guest, disks, fleecing state, config values, PVE and binary version |
| `dump <vmid>` | the `vzdump --stdout` VMA stream on stdout, the vzdump log on stderr |

`<vmid>` is a decimal guest id from 100 to 999999999 and must be in `VMIDS`. A dump runs
`nice -n 19 vzdump <vmid> --mode snapshot --stdout --compress 0 --bwlimit <BWLIMIT_KIB>
--ionice 7 --performance max-workers=<MAX_WORKERS> --fleecing enabled=1,storage=<FLEECING_STORAGE>`
with `PATH=/usr/sbin:/usr/bin:/sbin:/bin`. It holds `/run/restic-pve.lock` for its whole run, so
only one dump runs at a time. Before it starts, the fleecing storage must have at least
`FLEECING_MIN_FREE_GIB` free; while it runs, the free space is checked every 15 seconds, and
vzdump is stopped below `FLEECING_ABORT_FREE_GIB`.

Any other request, a request with the wrong number of arguments, an invalid vmid and an empty
command are refused with exit 64 and one line on stderr, for example
`restic-pve: refused: VM 105 is not on the allow list`.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | ok |
| 2 | the guest does not exist |
| 64 | refused |
| 73 | fleecing storage below its free-space limit |
| 75 | busy: another dump holds the lock |
| 78 | `/etc/restic-pve.conf` missing or invalid |
| other | vzdump's own exit status |

## The settings file

`/etc/restic-pve.conf`, read by `info` and `dump`. Strict `KEY=value` lines, every key exactly
once; blank lines and lines starting with `#` are allowed.

```
FLEECING_STORAGE=local
FLEECING_MIN_FREE_GIB=32
FLEECING_ABORT_FREE_GIB=8
BWLIMIT_KIB=40960
MAX_WORKERS=1
VMIDS=100 101
```

| Key | Rule |
|---|---|
| `FLEECING_STORAGE` | a storage name (`^[A-Za-z0-9][A-Za-z0-9_.-]*$`) |
| `FLEECING_MIN_FREE_GIB`, `FLEECING_ABORT_FREE_GIB` | whole GiB, 1 to 999999; the abort value below the minimum |
| `BWLIMIT_KIB` | a positive whole number |
| `MAX_WORKERS` | 1 to 16 |
| `VMIDS` | the allow list: guest ids separated by single spaces, no duplicates |

A missing or invalid file is exit 78; a guest not in `VMIDS` is refused (64).

## The SSH and sudo entries

```
restrict,from="192.0.2.10",command="sudo -n /usr/local/sbin/restic-pve request $SSH_ORIGINAL_COMMAND" ssh-ed25519 AAAA...
restic-backup ALL=(root) NOPASSWD: /usr/local/sbin/restic-pve request, /usr/local/sbin/restic-pve request *
```

The first sudoers entry matches `request` alone, so an empty SSH command reaches the binary and
is refused with exit 64 (`no request (interactive login is not available)`).

## Tests

The wrapper tests run the test binary as the wrapper (helper process) against stub `qm`,
`pvesm`, `pveversion` and `vzdump` scripts in a temporary directory. They need no PVE host and no
root.

[Back to the index](../README.md)
