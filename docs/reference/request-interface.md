# The request interface

For developers: `restic-pve request`, the only thing the backup machine can call on the Proxmox
host. It covers the commands, the settings file, the SSH and sudo entries, and the exit codes.
The request `changes` and the `provider` field of `info` belong to changed reads, which the admin
turns on with `restic-pve install` (see [provider-plugin.md](provider-plugin.md)).

## Commands

The backup machine logs in as the backup user; the forced command passes whatever it sent to
`sudo -n /usr/local/sbin/restic-pve request`.

| Request | Output |
|---|---|
| `ping` | `ok` on stdout |
| `info <vmid>` | one JSON line on stdout: guest, disks, fleecing state, config values, PVE and binary version |
| `dump <vmid>` | the `vzdump --stdout` VMA stream on stdout, the vzdump log on stderr |
| `changes <vmid> probe\|use\|new` | one provider job: the changes stream on stdout ([stream-contract.md](stream-contract.md)), the vzdump log on stderr; reads one ack line on stdin |

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

### changes

`changes <vmid> <mode>` is exactly three words: `<vmid>` as for `dump` and in `VMIDS`, the mode
one of `probe`, `use`, `new`. Anything else is exit 64 (`usage: changes <vmid> probe|use|new`).
The backup identity cannot pass a storage, an option, a path or a vzdump argument: the request
builds them all. In order:

1. It takes `/run/restic-pve.lock` without waiting (75 while a dump or another job holds it) and
   checks the fleecing storage's free space (73), as `dump` does.
2. Readiness (exit 79, `plug-in not ready: <reason>; run restic-pve install on the Proxmox
   host`): not a cluster node (`/etc/pve/corosync.conf`; the fix says to answer no); the plug-in file must be restic-pve's (absent: `changed reads are off on this host`,
   with `... and answer yes to changed reads`; without the marker: the file is named), the
   storage entry `restic-pve` must exist, and `pvesm status --storage restic-pve` must show it
   active (else Proxmox did not load the plug-in). A plug-in file of another release is
   rewritten from this binary's copy now, under the lock, before vzdump starts.
3. It opens the job socket `/run/restic-pve/provider.sock` (folder 0700, socket 0600, root;
   a socket left by an interrupted job is removed first) and starts
   `nice -n 19 vzdump <vmid> --storage restic-pve --mode snapshot --bwlimit <BWLIMIT_KIB>
   --ionice 7 --performance max-workers=<MAX_WORKERS> --fleecing enabled=1,storage=<FLEECING_STORAGE>
   --protected 0 --prune-backups keep-all=1 --remove 0` (the last three switch off site defaults
   of `/etc/vzdump.conf` that would act after the ack; see
   [provider-plugin.md](provider-plugin.md#site-defaults-of-etcvzdumpconf)) with
   `PATH=/usr/sbin:/usr/bin:/sbin:/bin`; vzdump's output goes to stderr and it holds the
   lock too. The free space is watched as for `dump`.
4. The socket accepts one connection: the peer must run as root and be the started vzdump or
   below it (`SO_PEERCRED` and the parent chain in `/proc`). Any other process is answered
   `not a job started by restic-pve`, logged as a `WARN:` line, and the socket keeps waiting;
   after the one connection the socket is unlinked. Calls must come in vzdump's order and name
   the requested guest, type `qemu`.
5. `probe` and `use` answer `use` for every disk, `new` answers `new`. In `backup_vm` the
   request writes the header (random 128-bit job token, and as `prev` the token in
   `/run/restic-pve/job-<vmid>`, `""` when there is none), the config frames and one map per
   disk. A map is read from the NBD export only for a disk Proxmox reports `reuse` (`probe`) or
   when every disk is `reuse` (`use`); the socket must be the one qemu-server opens for this
   guest, `/run/qemu-server/<vmid>.backup-access.<pid>/<vmid>-nbd.backup-access`, and every
   changed extent must lie on 4 MiB boundaries (else exit 3, `change record not on 4 MiB
   boundaries`). In `use` each marked piece is read through the read limit (`BWLIMIT_KIB`, and
   vzdump's `bandwidth-limit` when lower), sent, and discarded on the export. Nothing else is
   read; nothing is written but the socket. Every write to stdout must make progress within
   10 minutes (stdout is switched to non-blocking for the job), else the job ends with `the
   backup machine read nothing of the stream for 10m0s`. A disk that was not in the query, or
   a size that differs, is refused.
6. After the end frame it waits for one ack line on stdin (`ack <token> ok <snapshot>`, `arm` or
   `probe`, as the stream contract says). The expected one is first recorded: the job's token
   goes to `/run/restic-pve/job-<vmid>` (0600 root, temp file and rename), since Proxmox's
   record of changes counts from this job once it succeeds; when that write fails, the job
   fails. Only then does `backup_vm` return; `fail`,
   another token or verb, a malformed line, the end of stdin, a signal or one hour without an
   ack make it fail, so vzdump reports the guest failed and Proxmox keeps its record. The task
   log gets one line with what really happened (`probe: nothing read or stored`, `armed: change
   record started, nothing stored`, `stored: confirmed by the backup machine as snapshot <id>`).
7. The socket is removed, and its folder when it holds no `job-<vmid>` file; the lock is
   released. Exit: vzdump's status; 3 for a
   broken change record or NBD export; 73 when the space watch stopped vzdump; 1 when vzdump
   ended 0 without a confirmed job.

`/run/restic-pve/job-<vmid>` is on tmpfs on purpose: a reboot also ends every guest's record
of changes, so Proxmox then reports the disks `new` and the next backup reads in full anyway. A
guest restarted without a reboot gets a new record too (`new`). A file that is not a token is
read as none. The request interface is unchanged by it: no new request, argument or setting.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | ok |
| 2 | the guest does not exist |
| 3 | `changes`: the change record or the NBD export broke a rule |
| 64 | refused |
| 73 | fleecing storage below its free-space limit |
| 75 | busy: another dump holds the lock |
| 78 | `/etc/restic-pve.conf` missing or invalid |
| 79 | `changes`: changed reads are not ready on this host (off, plug-in not loaded, entry missing) |
| other | vzdump's own exit status |

## The settings file

`/etc/restic-pve.conf`, read by `info`, `dump` and `changes`. Changed reads add no key to it: the
plug-in file and the storage entry are their state. Strict `KEY=value` lines, every key exactly
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

## The provider field of info

`info` ends with `"provider":{"plugin":"absent|current|different|foreign","storage":true|false,"active":true|false,"cluster":true|false,"api":"ok|newer|unsupported|unknown|unchecked"}`:
the plug-in file (absent, this binary's copy, another release's, not restic-pve's), whether the
storage entry `restic-pve` exists, whether `pvesm status` shows it active, whether this is a
cluster node, and, for a plug-in of ours, how Proxmox's storage API fits it (`newer`: Proxmox
warns about the older API in its commands, also on a `dump`'s log, until restic-pve is
updated; `unsupported`: Proxmox refuses it). Perl is asked only when the plug-in is there. `pvesm` is asked
only when the entry exists, so a host without changed reads runs no extra command.

## The SSH and sudo entries

```
restrict,from="192.0.2.10",command="sudo -n /usr/local/sbin/restic-pve request $SSH_ORIGINAL_COMMAND" ssh-ed25519 AAAA...
restic-backup ALL=(root) NOPASSWD: /usr/local/sbin/restic-pve request, /usr/local/sbin/restic-pve request *
```

The first sudoers entry matches `request` alone, so an empty SSH command reaches the binary and
is refused with exit 64 (`no request (interactive login is not available)`). Changed reads keep
this rule as it is: `changes` is one more request behind it.

## Tests

The wrapper tests run the test binary as the wrapper (helper process) against stub `qm`,
`pvesm`, `pveversion` and `vzdump` scripts in a temporary directory. They need no PVE host and no
root. For `changes` the stub vzdump runs the real plug-in file through the Perl stand-in of
vzdump (`internal/provider/testdata/vzdump-driver.pl`) against a fake NBD export with a dirty
bitmap (the test binary again); those tests need Perl and skip without it
([build-and-test.md](build-and-test.md#tests-that-need-restic-or-perl)).

[Back to the index](../README.md)
