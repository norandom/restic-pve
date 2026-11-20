# The Proxmox plug-in

For developers: `ResticPVEPlugin.pm`, the one Perl file restic-pve puts on a Proxmox host for
changed-block reads when the admin answers yes in `restic-pve install`, what it does, the calls
it passes on, how it is written, kept in step with the binary and removed, and how it is tested
without Proxmox.

## Why there is Perl on the host

Proxmox gives the record of changed blocks of a running guest only to a backup provider
plug-in, and plug-ins are Perl modules loaded from `/usr/share/perl5/PVE/Storage/Custom/`. The
file is that plug-in and nothing more. It holds no backup logic: each call of a backup job goes
as one JSON line to the `restic-pve` process that started the job, and the answer comes back.
It reads and writes no file, starts no program, and uses only core Perl modules
(`IO::Socket::UNIX`, `JSON::PP`, `MIME::Base64`).

The file is embedded in the binary (`internal/provider`) and is written only from that copy.

- Its first line is the marker `# restic-pve plug-in, protocol 1. Written by restic-pve from its
  embedded copy; do not edit. Removed by restic-pve uninstall.` A file at that path without the
  marker is not ours and is never touched.
- It contains `our $PROTOCOL = 1;` and no release version, so it changes only when its code
  changes.

## On the host

| Item | Written by | Removed by |
|---|---|---|
| `/usr/share/perl5/PVE/Storage/Custom/ResticPVEPlugin.pm` (0644 root) | `install`, yes; rewritten by `host-sync` after an update and by `request changes` before vzdump, both only when it is another release's copy | `install`, no; `uninstall` |
| storage entry `restic-pve` in `/etc/pve/storage.cfg` (`pvesm add resticpve restic-pve --content backup --nodes <node>`) | `install`, yes | `install`, no; `uninstall` (`pvesm remove restic-pve`, before the file) |
| `/run/restic-pve/provider.sock` (folder 0700, socket 0600) | `request changes`, for one job | the same request when the job ends; the next request, or a reboot (tmpfs), after a crash; `uninstall` |
| `/run/restic-pve/job-<vmid>` (0600): the token of the guest's last job that ended with a success ack, sent as `prev` in the next job's header | `request changes`, at the ack | a reboot (tmpfs); `install`, no; `uninstall` |

The file is written with a temp file `.ResticPVEPlugin.pm.new-*` in the same folder, fsync,
mode and owner, and one rename. Proxmox loads only names matching `^.*\.pm$` from that folder
(pve-storage `src/PVE/Storage.pm`: `dir_glob_foreach('/usr/share/perl5/PVE/Storage/Custom',
'.*\.pm$', ...)`, and pve-common `src/PVE/File.pm` matches `m/^($regex)$/`), so it never sees the
temp file. The Proxmox daemons (`pvestatd`, `pvedaemon`, `pveproxy`) are restarted with
`systemctl try-restart` only when the file appears or disappears. A file at that path without
the marker line, and a storage named `restic-pve` of another type, are never touched.

Checks around the write (`internal/provider/proxmox.go`, through the same command seam as
`pvesm`, each Perl run bounded by `timeout 60`):

- before: no `/etc/pve/corosync.conf` (cluster nodes are refused: `pvesm add` writes the entry
  cluster-wide, the file exists only on this node, so the other nodes would warn about an
  unknown type and a storage change made there could drop the entry); then
  `perl -e 'require PVE::Storage; require PVE::BackupProvider::Plugin::Base; print APIVER, APIAGE'`:
  the base class must exist and the API must be at least 11, and the plug-in's 17 must be at
  least `APIVER - APIAGE`. Otherwise "yes" is refused and nothing is written;
- after: `perl -e 'require PVE::Storage; PVE::Storage::Plugin->lookup("resticpve")'`, so
  Proxmox's own loader has accepted the file (its warnings are part of the message); then
  `pvesm add`. When either fails, the file is put back as it was (removed, or the earlier
  version byte for byte) and the daemons are reloaded again if they had seen it;
- `APIVER` above 17 (inside the window): Proxmox loads it but warns about the older API in every
  process that loads storage plug-ins. Install and `host-sync` print a note, `info` reports
  `"api":"newer"`, and the backup machine's classifier keeps exactly that line as info.

Daemons are reloaded with `systemctl reload-or-try-restart pvestatd.service pvedaemon.service
pveproxy.service`, as pve-manager's `debian/postinst` does (`deb-systemd-invoke
reload-or-try-restart pvedaemon.service || true`, the same for pvestatd and pveproxy). A
failed reload leaves `/run/restic-pve.reload-pending`, and the next install or uninstall
reloads again.

The plug-in never changes under a running backup: every write happens under
`/run/restic-pve.lock`. A job that runs keeps the Perl code vzdump loaded at its start; the
next job starts with the plug-in of the binary that starts it.

## What it contains

Two packages in one file:

| Package | Base class | What it does |
|---|---|---|
| `PVE::Storage::Custom::ResticPVEPlugin` | `PVE::Storage::Plugin` | the storage type `resticpve`: content `backup`, feature `backup-provider`, options `nodes`, `disable`, `content`; `status` is active with no space; it lists no volumes and prunes nothing; every volume method dies with `restic-pve: this storage holds no volumes` |
| `PVE::BackupProvider::Plugin::ResticPVE` | `PVE::BackupProvider::Plugin::Base` | the provider `restic-pve`: passes each backup call on; restore and container methods die (`restic-pve: restore with ./restore.sh on the backup machine`) |

`api` returns the running storage API version when it lies between 11 (the first with backup
providers) and 17 (the highest checked), else 17. Proxmox's loader then accepts it while 17 is
inside its age window, and refuses it with its own message otherwise.

## The calls

The plug-in connects in `job_init` to `/run/restic-pve/provider.sock`, the root-only socket of
the running `restic-pve request changes`, and keeps that one connection until `job_cleanup`.
When the socket is missing or the server refuses the job, `job_init` dies with:

```
restic-pve: this storage takes only backups started by restic-pve on the backup machine; use another storage for Proxmox's own backup jobs
```

A job started from the Proxmox screens or Proxmox's own schedule therefore stops before any
guest is touched.

Each call is one line `{"method":"<name>","args":{...}}`; each answer is one line
`{"ok":true,"result":...,"log":"..."}` or `{"ok":false,"message":"..."}`. `ok:false` makes the
method die with `restic-pve: <message>`. `log`, when present, is written to the task log as
one `info` line. Answers are at most 1 MiB.

| Method (vzdump's order) | args | result returned to vzdump |
|---|---|---|
| `job_init` | `protocol` (1), `start_time` | none |
| `backup_init` | `vmid`, `type`, `start_time` | `{"archive-name":"..."}` |
| `backup_get_mechanism` | `vmid`, `type` | `"nbd"` |
| `backup_vm_query_incremental` | `vmid`, `volumes` (device: `size`) | device: `use` or `new` |
| `backup_vm` | `vmid`, `config_b64`, `firewall_b64` (when present), `volumes` (device: `size`, `bitmap-mode`, `bitmap-name`, `nbd-path`), `bandwidth_limit` (bytes/s, 0 none) | none; returns only when the server answers |
| `backup_cleanup` | `vmid`, `type`, `success`, `error` (on failure) | `{"stats":{"archive-size":N}}` |
| `backup_handle_log_file` | `vmid`, `filename` | none |
| `job_cleanup` | none | none |

`backup_cleanup` comes before `backup_handle_log_file`, and after a failure it is called with
`success` false (also after a call with `success` true that was followed by a failure).
`provider_name` is answered by the plug-in itself. When the connection is lost, the call dies
with `restic-pve: connection lost in <method>`, later calls die with `restic-pve: no job
connection for <method>`, and a broken pipe never ends the vzdump process.

## The job server

`restic-pve request changes` serves these calls on the socket for exactly one job
(`internal/provider/rpc.go`, `internal/wrapper/changes.go`; the full steps are in
[request-interface.md](request-interface.md#changes)): one connection, from a root process that
is the started vzdump or below it; the calls in the order above (a second guest, an unknown
method or a call out of order is refused); the guest must be the requested one, type `qemu`;
the NBD socket must be the one qemu-server opens for the guest; `backup_vm` returns only after
the backup machine's ack for this job, and `backup_cleanup` refuses a `success` that no ack
confirmed. Each request line is at most 4 MiB, and the wait for the next call one hour.

The plug-in takes no part in `prev`: the job server writes and reads the file, so the
plug-in file is the same as before it and `host-sync` does not rewrite it.

## What Proxmox records for these jobs

Proxmox reports a job to `restic-pve` as a successful backup when the plug-in's calls succeed,
and that happens after the backup machine's ack for the job: `ok` (stored and checked), but
also `probe` (nothing read or stored) and `arm` (the change record started, nothing stored). An
ack proves the session, not that anything was stored. The plug-in gives Proxmox's task log one
info line with the truth, through the log function vzdump hands to the provider (the reply to
`backup_vm`): `restic-pve: probe: nothing read or stored (...)`, `restic-pve: armed: change
record started, nothing stored (...)`, `restic-pve: stored: confirmed by the backup machine as
snapshot <id> (...)`. `backup_handle_log_file` cannot do this: it hands the provider the path of
the finished task log for the provider to keep, and the plug-in neither reads nor writes files.

## Site defaults of /etc/vzdump.conf

vzdump applies `/etc/vzdump.conf` to these jobs too. The request passes
`--protected 0 --prune-backups keep-all=1 --remove 0`: pve-guest-common
`src/PVE/VZDump/Common.pm` describes `protected` as "If true, mark backup(s) as protected.",
`prune-backups` with default "keep-all=1", and `remove` as "Prune older backups according to
'prune-backups'."; pve-manager `PVE/VZDump.pm` marks protected and adds notes after the archive
step (`PVE::Storage::update_volume_attribute($cfg, $volid, 'protected', 1)`, `... 'notes',
$notes`) and sets `$opts->{remove} = 0 if $opts->{'prune-backups'}->{'keep-all'};`. Without the
options, `protected: 1` would fail the job after the backup machine stored and acked it. A
`notes-template` cannot be switched off this way; the plug-in's `update_volume_attribute`
does nothing, so it is harmless. A hook script (`script:`) runs as for every vzdump.

## Sources it was checked against

git.proxmox.com, master branch of 2026-11-20:

- pve-storage `src/PVE/BackupProvider/Plugin/Base.pm`: the 21 methods and their arguments,
  `backup_init` returning `archive-name`, `backup_cleanup` returning `stats`, the volume keys
  `bitmap-mode` (`none`, `new`, `reuse`), `nbd-path`, `bitmap-name`, and `bandwidth-limit` in
  bytes per second.
- pve-storage `src/PVE/Storage.pm`: the loader (`require`, `isa('PVE::Storage::Plugin')`,
  `api()` between `APIVER - APIAGE` and `APIVER`, then `register`), `APIVER` 17 and `APIAGE` 8,
  and `new_backup_provider($scfg, $storeid, $log_function)`. The example plug-in in
  pve-storage-plugin-examples still passes a `$bandwidth_limit` argument there; Storage.pm does
  not.
- pve-storage `src/PVE/Storage/Plugin.pm`: `status` returns `(total, avail, used, active)`.
- pve-manager `PVE/VZDump.pm` and qemu-server `src/PVE/VZDump/QemuServer.pm`: the call order,
  the checks of each result, the NBD socket below `/run/qemu-server/<vmid>.backup-access.<pid>/`,
  and that the export is opened writable for discards.
- Checked again on 2026-11-20 (master): qemu-server sets
  `$task->{'backup-access-root-dir'} = "/run/qemu-server/${vmid}.backup-access.$$/";` and
  `my $nbd_path = "$task->{'backup-access-root-dir'}/${vmid}-nbd.backup-access";`, uses
  `my $target_id = "snapshot-access:$opts->{storage}";` as the bitmap name, refuses
  `"cannot setup backup access without fleecing\n"`, and passes
  `$param->{'bandwidth-limit'} = $opts->{bwlimit} * 1024`; pve-manager calls
  `backup_cleanup($vmid, $vmtype, 1, {})`, after an error `backup_cleanup(..., 0, { error => $err })`,
  then `backup_handle_log_file`, and `job_cleanup` (again after an error); pve-storage's
  `pvesm` has `add => [..., 'create', ['type', 'storage']]` and `remove => [..., 'delete', ['storage']]`,
  and a plug-in that fails to load is only warned about (`warn "Error loading storage plugin ..."`).

## Tests without Proxmox

`internal/provider/plugin_test.go`, with Perl stubs of `PVE::Storage`, `PVE::Storage::Plugin`
and `PVE::BackupProvider::Plugin::Base` in `testdata/lib`:

- Without Perl: the embedded copy equals the file; marker, protocol and refusal text; at most
  220 lines; only the allowed modules; no program, file or process call of any kind.
- With Perl: `perl -c`; the loader's version window (API 10, 11, 17, 20, 30); and
  `testdata/vzdump-driver.pl`, which loads the file as Proxmox's loader does and calls it in
  vzdump's order with vzdump's checks against a stub job server: a whole job, a failed backup,
  no socket (the refusal), a refused job, a lost connection, and answers vzdump must reject.

The job server, install, update and the request have their own tests: `hostfiles_test.go` and
`rpc_test.go` here (the plug-in file's states and writes, the order "entry before file", the
socket, the peer check, the call order), `internal/install` (the question, yes, no, reinstall,
uninstall, a held lock), `internal/update` (host-sync after the rename), and
`internal/wrapper/changes_test.go`, where the stub vzdump runs this driver with the real plug-in
against a fake NBD export: probe, use, new, every kind of missing or wrong ack, a lost
connection, vzdump failing, a misaligned record, a foreign connection, a second job, a stale
socket, a signal, the read limit, and the plug-in sync before a job.

How to run them is in [build-and-test.md](build-and-test.md#tests-that-need-restic-or-perl).

Only a real Proxmox host can confirm:

- that Proxmox's loader accepts two packages in one file;
- whether `pvestatd`, `pvedaemon` and `pveproxy` need a restart before the screens know the
  storage type, and that the plug-in adds no log lines there;
- that 9.2's real vzdump calls the methods with the arguments above (the driver follows the
  source, not a running vzdump);
- that the refusal text appears in the task log of a job started from the Proxmox screens;
- that the load check and `reload-or-try-restart` behave as on the stand-ins, and that the
  `restic-pve: probe|armed|stored` line shows in the task log;
- that `--protected 0 --prune-backups keep-all=1 --remove 0` win over `/etc/vzdump.conf`;
- that vzdump removes its fleecing image after a provider job that failed or was stopped
  (restic-pve creates none itself);
- what happens to a running job when the host reboots (the job ends; the socket goes with the
  tmpfs; the record should stay as before the job).

[Back to the index](../README.md)
