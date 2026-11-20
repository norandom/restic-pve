# The Proxmox plug-in

For developers: `ResticPVEPlugin.pm`, the one Perl file restic-pve will put on a Proxmox host
for changed-block reads, what it does, the calls it passes on, and how it is tested without
Proxmox. It is built and tested, but no release writes it to a host yet.

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

## Tests without Proxmox

`internal/provider/plugin_test.go`, with Perl stubs of `PVE::Storage`, `PVE::Storage::Plugin`
and `PVE::BackupProvider::Plugin::Base` in `testdata/lib`:

- Without Perl: the embedded copy equals the file; marker, protocol and refusal text; at most
  220 lines; only the allowed modules; no program, file or process call of any kind.
- With Perl: `perl -c`; the loader's version window (API 10, 11, 17, 20, 30); and
  `testdata/vzdump-driver.pl`, which loads the file as Proxmox's loader does and calls it in
  vzdump's order with vzdump's checks against a stub job server: a whole job, a failed backup,
  no socket (the refusal), a refused job, a lost connection, and answers vzdump must reject.

How to run them is in [build-and-test.md](build-and-test.md#tests-that-need-restic-or-perl).

Only a real Proxmox host can confirm:

- that Proxmox's loader accepts two packages in one file;
- whether `pvestatd`, `pvedaemon` and `pveproxy` need a restart before the screens know the
  storage type, and that the plug-in adds no log lines there;
- that 9.2's real vzdump calls the methods with the arguments above (the driver follows the
  source, not a running vzdump);
- that the refusal text appears in the task log of a job started from the Proxmox screens.

[Back to the index](../README.md)
