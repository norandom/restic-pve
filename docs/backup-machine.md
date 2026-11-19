# restic-pve on the backup machine

For an admin who looks after the backups on the backup machine and wants to use `status`,
`keys`, `space` and `watch`. Each section is one question and the command that answers it.

## What is here and what is not

On the backup machine this repository provides five commands: `status`, `keys`, `space`,
`watch` and `stream`. They read the backup machine's files and its storage; none of them changes
a backup. The rest of the backup machine is control code that is not published yet: it holds
the schedule, calls restic and rclone, writes the records these screens read, and provides the
setup. Making the backup machine installable from this repository is the next stage (see the
[README](../README.md#state-and-roadmap)).

So on a machine without that control code, these screens have nothing to show yet. The paths
some screens name, such as `docs/guide/07-restore.md` or `./backup.sh`, belong to the control
code as well.

## What the commands need

The tool counts as the backup machine when `RESTIC_PVE_STATE` is set. Each command says which
variable is missing and gives an example (exit 78). In the container image of the control code
these are set already.

| Variable | Needed by | Example |
|---|---|---|
| `RESTIC_PVE_STATE` | all | `/state` |
| `BACKUP_CONFIG` | status, space; keys reads it if set | `/app/config/backup.toml` |
| `RESTIC_PASSWORD` | status, space (not `space --check`) | from `.env` |
| `RESTIC_PVE_KEYS` | keys | `/config` |
| `RESTIC_PVE_START_HINT` | status, watch | `'./backup.sh <source>'` |
| `RCLONE_CONFIG` | read by rclone; keys shows the section name | |

The full list, with what each command reads: [reference/commands.md](reference/commands.md).
The screens read the storage at most every six hours, or when you press `r`, at low CPU and I/O
priority. Looking never changes anything.

## Is there a recent copy away from the site?

```sh
restic-pve status
```

![Status, all safe: three sources with a copy two to four days old, the restore test passed, and the line All copies are recent and checked](img/status-ok.png)

Three boxes answer three questions: is there a recent copy away from the site, have I seen a
restore work, and what runs now and next. The line under them is the verdict.

![Status with a warning: the source docs has no checked copy for 21 days, and the bottom line says Do: ./backup.sh docs](img/status-warning.png)

A source that is not safe comes first, in red, and the bottom line gives the one command that
fixes it. Enter lists all rows, `k`, `s` and `w` open keys, space and watch, `r` reads the
storage again, `?` shows the help.

In scripts, `restic-pve status --plain` prints one fact per line and exits 0 when every source
and guest is safe, 1 when one is not or is not known.

## Which keys matter, and is my password copy right?

```sh
restic-pve keys
```

![Keys: repository password, storage sign-in, Proxmox access, guest health and alert address, each with what it does and what losing it means](img/keys.png)

Every key on this machine, by presence only: no value is shown. Only the repository password
needs a copy away from this machine; losing both it and the copy means nobody can read the
backups. The release signing key is not on this machine; it belongs to the maintainer.

Press `p` to prove the copy in your password manager. Paste it and press Enter; the field shows
the same six dots whatever you paste.

![Password check refused: This copy is not the password in use, nothing was written](img/keys-proof-wrong.png)

A wrong copy is refused, and nothing is written.

![Password check passed: Your copy opens all 3 repositories, proven today](img/keys-proof-ok.png)

The right copy opens every repository, and the status screen then shows the proof date.

## Is there enough space on the storage?

```sh
restic-pve space
```

![Space: total, used and free on the storage, backups and how long they are kept, and no warning](img/space.png)

Storage as the drive reports it, the size of each backup and its keep rules, and the warning
rule with today's numbers.

![Space with the warning: only 20 GiB free, LOW NOW, lower the keep rules or add storage](img/space-warning.png)

The warning holds when the free space is below twice the largest recent run, or below a tenth of
the total. `restic-pve space --check` prints that verdict as one line (exit 0 no warning, 1
warning, 2 not known); the control code runs it after each backup.

## What runs right now?

```sh
restic-pve watch
```

![Live block view halfway: a yellow run of blocks in the backup, a few white blocks read ahead, dots not read yet, 30 MiB/s on the bottom bar](img/watch-running.png)

The guest backup that runs now, as blocks. `r` marks the part being read from the Proxmox host,
`W` data going into the backup, and the yellow run what is already in it. Nothing is animated:
when the backup waits, the picture waits. `q` leaves the view, and the backup goes on.

![Live block view at the end: one solid yellow run, Done in 00:00:32, restic now completes the snapshot](img/watch-done.png)

At the end the run is solid and the status box says how long it took; restic then completes its
snapshot. For a local source (files, not a guest) watch shows its progress instead. When nothing
runs, it names the command that starts a backup. It closes itself ten minutes after the last
running backup. `restic-pve watch --once` prints one line instead.

[Back to the index](README.md)
