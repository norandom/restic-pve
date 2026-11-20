# Commands

For developers and for scripts: every subcommand of `restic-pve` per role, its flags, what it
reads and its exit codes. The admin view of the same commands is in [../host.md](../host.md) and
[../backup-machine.md](../backup-machine.md).

## Roles

The tool decides which machine it runs on:

- Proxmox host: `/etc/pve` is a folder, or `/etc/restic-pve.conf` exists.
- Backup machine: the environment variable `RESTIC_PVE_STATE` is set (the state folder, for
  example `/state`).
- When both hold, it is the Proxmox host, so a Proxmox host never runs a backup-role command by
  accident.

A command of the other role is refused before it does anything, with exit 64 and one line, for
example `restic-pve install is for the Proxmox host; this is the backup machine. Nothing was
changed. Run it in a root shell on the Proxmox host.` When neither fact holds, the host commands
run, and a backup-role command says to set `RESTIC_PVE_STATE` (exit 78). `restic-pve` without a
command prints the usage of this machine's role (exit 64).

## Host role

| Command | Who | What |
|---|---|---|
| `request ping` | backup identity (forced command, sudo) | prints `ok` |
| `request info <vmid>` | backup identity | one JSON line: guest, disks, fleecing state, config values, PVE and binary version |
| `request dump <vmid>` | backup identity | `vzdump --stdout` VMA stream on stdout, vzdump log on stderr |
| `request changes <vmid> probe\|use\|new` | backup identity | one provider job for changed reads: the changes stream on stdout, the vzdump log on stderr, the ack line on stdin |
| `install [--plain]` | root | asks for the site values (form, or line by line), installs or changes the identity |
| `update` | root | replaces itself with the latest signed release |
| `uninstall` | root | removes the identity, its files and the binary |
| `host-sync` | root (run by `update`) | brings the Proxmox plug-in to this binary's copy; not in the usage line |

`request` is described in [request-interface.md](request-interface.md).

### install

`install` takes no argument or `--plain` (anything else is exit 64). It runs as root on a PVE
host only: when `qm` or `pvesm` is not in `/usr/sbin:/usr/bin:/sbin:/bin` it stops with one line
(exit 1). Exit 0 installed; exit 1 not root, not a PVE host, stopped before the install (nothing
changed), or a step failed. The values and their rules:

1. backup machine address for `from=` (`^[A-Za-z0-9.:,*?!/_-]{1,255}$`)
2. public key, one line `ssh-ed25519 <base64> [comment]` (a real ed25519 key blob; only type and
   key are stored)
3. guests, each once, each listed by `qm list`, at least one
4. fleecing storage, one of the active storages of `pvesm status`
5. the limits: `FLEECING_MIN_FREE_GIB`, `FLEECING_ABORT_FREE_GIB` (below the first), the read
   limit in MiB/s (stored as `BWLIMIT_KIB`), `MAX_WORKERS` (1-16)
6. changed reads, yes or no (default: no on a fresh host, the current state on an installed
   one). Yes adds the Proxmox plug-in and the storage entry `restic-pve`; no removes both.
   The storage `restic-pve` is never offered as a fleecing storage. Yes is refused before
   anything changes on a cluster node, on a Proxmox without the backup provider interface, and
   on one that no longer accepts the plug-in (`restic-pve: changed reads: <reason>; nothing
   changed`, exit 1).

Form. When stdin and stdout are a terminal, `TERM` is set and not `dumb`, the terminal is at
least 60x20 and `--plain` is not given, install shows a full-screen form in the retro look
([house-style.md](house-style.md)). One screen per value: text fields for address and key (an
empty field shows a grey example that is never taken as the value), a checklist of `qm list`
(Up/Down or j/k, Space ticks), a single choice of the active storages with free GiB, the limits
with "Keep these (recommended)" or "Change" (four number fields; the Help box shows the range of
the field being edited), and a summary with `[ Install ]  [ Back ]  [ Cancel ]`, focus on
Install. Enter continues when the value is valid, else its rule shows in the Help box and the
screen stays. Esc or Shift-Tab goes one screen back with the values kept. Ctrl-C or Cancel is
exit 1 and nothing changed. A pasted key with a trailing newline counts as Enter; bracketed
paste is understood. The terminal is restored on exit, Ctrl-C, SIGTERM, SIGHUP and panic.
`NO_COLOR` keeps the layout without colours (focus in inverse video); a locale that is not UTF-8
gets ASCII frames. The changed-reads screen explains in three sentences what is added, why, and
how to remove it, and offers "No" (the cursor starts on the current state) and "Yes".

Plain (`--plain`, or no usable terminal). One answer per line from stdin, the questions on
stdout, with an example for address and key. Enter takes the default in brackets. An invalid
answer prints its rule and the question comes again; three invalid answers, or the end of stdin
(`no terminal to ask on`), stop with exit 1 and nothing changed. The guests are typed as VM
numbers separated by spaces or commas; the limits follow `Change the limits? [y/N]`; then the
same three sentences and `Read only the changed parts of running guests? [y/N]` (`[Y/n]` when
changed reads are on). At the end `Install with these values? [y/N]`: anything but `y` is exit 1
and nothing changed.

Defaults on a fresh host: no address, no key, no guests, no storage, limits 32 / 8 GiB,
40 MiB/s, 1 worker. On an installed host they are the current values: address and key from the
backup user's `authorized_keys`, the rest from `/etc/restic-pve.conf`. The user is
`restic-backup`; a different user in an existing `/etc/sudoers.d/restic-pve` (older install) is
kept.

Install is idempotent and reports each file as `written` or `unchanged`: the sudo package (only
if missing), the user (shell `/bin/sh`, password locked), `~/.ssh/authorized_keys`,
`/etc/sudoers.d/restic-pve` (checked with `visudo -cf` first), the binary at
`/usr/local/sbin/restic-pve`, `/etc/restic-pve.conf`, and the automatic update:
`/etc/systemd/system/restic-pve-update.service` (oneshot, `restic-pve update`) and
`/etc/systemd/system/restic-pve-update.timer` (daily, random delay up to 6 h, persistent), both
compiled in from `internal/install/units/`. It then runs `systemctl daemon-reload` and
`systemctl enable --now restic-pve-update.timer`. The timer is always on. Last, it removes the
files of the bash wrapper if present: `/usr/local/sbin/restic-vzdump`,
`/etc/sudoers.d/restic-vzdump`, `/etc/restic-vzdump.conf` and `/etc/restic-vzdump.allow`.

Changed reads, last. Yes: the plug-in file
`/usr/share/perl5/PVE/Storage/Custom/ResticPVEPlugin.pm` is written from the binary's own copy
(temp file `.ResticPVEPlugin.pm.new-*` in the same folder, fsync, mode 0644 root, the folder
0755 whatever the umask, rename; reported `written` or `unchanged`); the load check; when the
file was not there before (or a reload is still owed),
`systemctl reload-or-try-restart pvestatd.service pvedaemon.service pveproxy.service`
(`reloaded`); then `pvesm add resticpve restic-pve --content backup --nodes <this node>` when
the entry is missing (`added`, else `unchanged`). A failed load check or `pvesm add` puts the
file back as it was (`restored`) and says what failed. A newer Proxmox gives a `note` line. No:
`pvesm remove restic-pve` first, then the file, the empty `Custom/` folder, the reload, and
`/run/restic-pve/` (each `removed` or `absent`). A file there
without restic-pve's marker line, or a storage named `restic-pve` of another type, is never
touched: yes stops with the reason, no reports it as `kept`. When this step would change
something and a backup holds `/run/restic-pve.lock`, install stops before anything changes
(exit 1, `run restic-pve install again when it has finished`).

`install` reads the answers or keys from its own stdin, the terminal. Downloaded straight to
`/usr/local/sbin/restic-pve` and run from there, install leaves the file in place and only sets
owner and mode. Run from any other path, it copies itself there.

### uninstall

`uninstall` takes no argument. It refuses with exit 75 while a dump holds the lock. Otherwise it
first removes changed reads as install's "no" does (the storage entry through `pvesm remove`, the
plug-in file, the empty `Custom/` folder, the daemon reload, `/run/restic-pve/`). When `pvesm
remove` or the reload fails, it goes on with everything else, keeps the binary
(`kept`), and ends with `uninstall not complete: left on this host: <what>. To finish: <command>,
then restic-pve uninstall` (exit 1). After changed reads it
runs `systemctl disable --now restic-pve-update.timer` (if the timer file is there) and removes,
each reported as `removed` or `absent`: both update units (then `systemctl daemon-reload`),
`/etc/sudoers.d/restic-pve`, `/etc/restic-pve.conf`, the four bash wrapper files, the lock file,
the backup user with its home (`userdel --remove`; the name comes from the sudoers entry, and
when that entry is missing or not the one install writes, users are left alone), and last the
binary. The sudo package stays. Exit 0 done, 1 not root or a step failed, 75 lock held.

### update

`update` takes no argument and needs root. It downloads from
`https://github.com/<repo>/releases/latest/download/`, verifies `SHA256SUMS.sig` against the
Ed25519 keys compiled into the binary (`internal/relkeys`), checks the hashes of `VERSION` and
the binary, requires a strictly newer version, runs the new binary with `version`, and renames
it over the installed one. Downloads must stay on HTTPS (a redirect to another scheme is
refused); the binary may be at most 64 MiB. Any failure leaves the installed binary untouched and
exits 1, so the timer's service shows as failed. Up to date is one line and exit 0. A build
without keys refuses to update. The rename is atomic: a dump that is running keeps the old file
open and finishes with it.

After the rename, `update` runs the new binary as `restic-pve host-sync` (one minute at most) and
passes its lines on. `host-sync` takes `/run/restic-pve.lock` without waiting. Free: when the
plug-in file is restic-pve's but another version, it is rewritten (`written`), else `unchanged`;
when changed reads are off (no file) it says so and creates nothing. Held by a backup: it changes
nothing and says the next backup brings the plug-in up to date (exit 0). A plug-in file without
the marker is refused (exit 1). On a cluster node, and when Proxmox's storage API is newer than
the plug-in was checked against or no longer accepts it, it adds a line that says so. The
update counts as done either way; the next
`request changes` syncs the file under the lock before it starts vzdump.

## Backup role

| Command | Who | What |
|---|---|---|
| `stream backup\|verify\|to-vma\|to-raw` | the backup machine's control code | the stream program, see [stream-contract.md](stream-contract.md) |
| `status [--plain]` | the admin, scripts | is there a recent copy away from the site, have I seen a restore work, what runs now and next |
| `keys [--plain]` | the admin | every key of this machine, what it does, where it is, which one needs a copy away; `p` proves the password copy |
| `space [--plain\|--check]` | the admin; `--check` the control code | storage space, backup sizes, keep rules, the warning rule |
| `watch [--once]` | the admin | the guest backup that runs now as the block view, or the progress of a local source |
| `pieces --job FILE` | the backup machine's control code; the operator for a probe | one guest run in the piece layout; this release runs the probe only |

A wrong flag, or more than one flag, prints the usage line of that command (exit 64).

### What the screens read

They never change anything by looking. The storage is read at most every six hours, or on `r`,
with restic and rclone at nice 19 and idle I/O priority; while a backup runs, an existing cache
is shown and the read waits.

| Variable | Read by | Meaning, example |
|---|---|---|
| `RESTIC_PVE_STATE` | all | state root: `records.json`, `status.json`, `jobs/<name>.json` and `.lock`, `run.lock`, `guests/` for the guest profile, `status-cache.json` (written), `password-escrow-confirmed` (written by the proof); `/state` |
| `BACKUP_CONFIG` | status, space (required), keys (if set) | `backup.toml`: `[repository] base`, `[[source]]`, `[retention]`, `keep_last`, `[alerts] stale_days`, `[schedule]`; `/app/config/backup.toml` |
| `RESTIC_PASSWORD` | status, space (required, not for `--check`), keys | the password in use, passed to restic in its environment only |
| `RESTIC_REPOSITORY_BASE` | status, keys, space | overrides `[repository] base` |
| `RCLONE_CONFIG` | keys (section name only), restic and rclone children | the rclone config |
| `RESTIC_PVE_KEYS` | keys (required) | folder with `ssh/id_ed25519` and `guest-ssh/id_ed25519` (checked by existence); `/config` |
| `RESTIC_PVE_START_HINT` | status, watch (required) | the start command in the actions; `'./backup.sh <source>'` |
| `DISCORD_WEBHOOK_URL`, `RUNNER_TOKEN`, `OPENOBSERVE_USER`, `OPENOBSERVE_PASSWORD` | status, keys | set or not set only; values are never read into output |
| `TERM`, `NO_COLOR`, `TZ` | all screens | `TERM=dumb` gives plain lines, `NO_COLOR` no colours, times in `TZ` |

A required variable that is missing, or a config that cannot be read, is exit 78 with one line
that names it and gives an example.

### status

`status --plain` (or no terminal, `TERM=dumb`, a terminal below 60x20) prints one fact per line,
words separated by two spaces: `copy <instance> yes|no|unknown <age> ...`, `restore-test`,
`drill`, `running`, `last-job`, `next`, `space`, `updates`, `alerts`, `password-proof`,
`problem`, `verdict safe|not-safe|unknown`. Exit codes: 0 every source and guest is safe, 1 at
least one is not safe or not known, 64 usage, 78 a variable is missing or the config cannot be
read. The full screen exits with the same code and prints the verdict line when it closes.

### keys

`keys --plain` prints the key table. Without a terminal it ends with a `proof` line that says a
terminal is needed; on a terminal it offers the proof (`p` and Enter), reads the copy without
echo, and exits 0 when the copy is proven, 1 when not.

### space

`space --plain` prints the space facts. `space --check` reads the free space with one
`rclone about` (never restic) and prints one line: exit 0 no warning, 1 warning (free below twice
the largest recent run, or below a tenth of the total), 2 not known.

### watch

`watch` reads `<RESTIC_PVE_STATE>/guests/view` (the folder `stream backup --view-dir` writes) and
`<RESTIC_PVE_STATE>/status.json` for a local source, and never writes. When no backup runs it
shows `RESTIC_PVE_START_HINT`, the command that starts one on this machine; without that
variable it stops with exit 78 and names it. It closes itself ten minutes after the last running
backup; leaving it never stops a backup. `--once`, no terminal, `TERM=dumb` or a small terminal
print one plain line (exit 0).

### pieces

`pieces --job FILE` takes exactly that flag (anything else: usage line, exit 64). The job file is
JSON, schema 1, at most 1 MiB, every field known:

```json
{"schema":1,"mode":"probe","vmid":100,"alias":"web","source":"site1",
 "ssh_config":"/config/ssh_config","ssh_host":"pve","restic_host":"backup",
 "tags":["source=site1","unit=guest-web","kind=guest-image","vmid=100"],
 "info":{"vmid":100},"view_dir":"/state/guests/view","progress_interval_s":30,
 "deadline":"2026-11-21T03:00:00Z","full_check_every":7}
```

`mode` is `probe`, `auto` or `full-check`; this release runs `probe` only and refuses the others
with exit 64 and nothing changed. A broken job file is exit 64 with the field named.

The probe runs `ssh -F <ssh_config> -T -o BatchMode=yes <ssh_host> changes <vmid> probe` with
every variable that holds or points to a secret removed from ssh's environment. It reads the
changes stream (header, config, one map per disk, end), writes the view file
`<view_dir>/live` (kind `probe`, see [house-style.md](house-style.md)), sets the guest's record
state `<RESTIC_PVE_STATE>/pieces/<alias>.record.json` to `none` (written before the ack: Proxmox
now counts changes from this job, past any earlier base), sends `ack <token> probe`, and ends
with the result line on stderr:

```json
{"type":"result","status":"ok","code":0,"transient":false,
 "message":"probe: drive-efidisk0 record reuse, 0 KiB changed of 528 KiB; drive-scsi0 record reuse, 388 MiB changed of 32.0 GiB; nothing was read or stored",
 "snapshot":null,"kind":"probe","read_bytes":0,"skipped_bytes":33953431552,"stored_bytes":0,
 "disks":[{"name":"drive-efidisk0","record":"reuse","dirty_bytes":0},{"name":"drive-scsi0","record":"reuse","dirty_bytes":406847488}],
 "fallback":null,"check":"none"}
```

`record` is what Proxmox reported per disk: `reuse` (a record exists; `dirty_bytes` is what it
marks), `new` or `none` (no record yet; `dirty_bytes` is the whole disk, which the next run reads).
Before it, the vzdump log goes through the classifier of `stream backup`: one `consistency`
line, and `vzdump` lines for warnings and errors only (the plug-in's and the provider job's
lines are info). It reads no disk data and runs no restic command. Codes: 0 ok, 3 the stream
broke a rule, 6 ssh or the Proxmox side failed (`transient` as for `stream backup`), 7 the record
state could not be written (the host then gets `ack <token> fail ...`), 64 job file, 79 the
Proxmox plug-in is not ready (the message says what to do on the host).

On a terminal the block view of the probe shows while it runs and after it ends, until `q`; the
JSON lines follow on stderr when the view is left, and the result message on stdout. Without a
terminal (or `TERM=dumb`, a small terminal) only the result message goes to stdout.

## Both roles

| Command | What |
|---|---|
| `version` | prints the version on stdout exactly (`vX.Y.Z` and a newline; hosts compare it during their self-update). When stderr is a terminal it adds the role on stderr, for example `role: backup machine` |
| `demo [pack] [full\|incremental]` | the block view with a simulated run of a 125 GiB disk; reads no data, needs a terminal, not in the usage line. Without `pack` the blocks turn yellow in place; with `pack` they are packed into one run |

[Back to the index](../README.md)
