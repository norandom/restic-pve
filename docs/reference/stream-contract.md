# The stream contract

For developers who call `restic-pve stream` or change it: its subcommands, flags, output, the
result line and the exit codes. The backup machine's control code parses this contract, so any
change to the flags, the exit codes or the result line is a change of the interface.

`restic-pve stream` is the former `vmastream` program of the backup machine, moved here with its
contract unchanged.

## Why it exists

A live VMA stream sends disk clusters in whatever order they arrive and interleaves several
devices, so two backups of an almost unchanged guest have little in common byte for byte.
`stream backup` reorders the clusters into plain per-device order (the RVS1 container).
Unchanged disk regions then give identical bytes, and restic deduplicates them. A full image is
never stored on disk; a bounded RAM reorder buffer handles out-of-order clusters.

| Command | Input | Output |
|---|---|---|
| `backup` | VMA via `ssh ... dump <id>` or `--input FILE` | RVS1 on stdout, manifest JSON (`--manifest-out`) |
| `verify` | RVS1 on stdin or `--in`; with `--pieces` the tar of a pieces snapshot | fingerprint check against trailer and `--manifest` |
| `to-vma` | the same | uncompressed VMA (`--out FILE` or `-`) |
| `to-raw` | the same | `<dir>/<device>.raw` (sparse) and `<dir>/SHA256SUMS` |

`restic-pve stream <command> -h` lists the flags. All hosts, ids and paths come from flags; the
command has no built-in site value and no default folder.

The role guard comes first: on a Proxmox host `stream` is refused with exit 64 and no result
line, and without `RESTIC_PVE_STATE` with exit 78.

## Environment

restic starts `stream backup` with restic's own environment. Before anything else, the command
removes every variable that holds or points to a secret (names containing `PASSWORD`,
`PASSPHRASE`, `SECRET`, `TOKEN`, `WEBHOOK` or `CREDENTIAL`, or ending in `_KEY` or `_PASS`,
`RESTIC_PASSWORD` among them) from its own environment, so `ssh` and anything else it starts
never receive them. `VMASTREAM_VIEW=off` turns the block view off whatever the flags say.

## backup

| Flag | Meaning |
|---|---|
| `--ssh-config` | ssh client config file |
| `--ssh-host` | ssh host alias of the hypervisor |
| `--vmid` | guest id (decimal) |
| `--manifest-out` | path of the manifest JSON written on success |
| `--input` | read the VMA stream from this file instead of ssh (tests) |
| `--ram-mib` | reorder buffer RAM cap in MiB, default 512 |
| `--progress-interval` | interval of progress lines, default 30s |
| `--view-dir` | write the block view's state file to this folder |
| `--view-name` | guest alias shown by the block view |

`backup` runs `ssh -F <config> -T -o BatchMode=yes <host> dump <vmid>` (fixed argv, no shell;
`ssh` is looked up in `PATH`). The far side is `restic-pve request dump` on the Proxmox host,
which writes the VMA stream to stdout and the vzdump log to stderr.

The pipeline is: VMA reader (all structural checks), then the reorder buffer, then the
per-device `sha256ct` fingerprint and the RVS1 writer on stdout. The trailer is written last,
only after the source exited cleanly. The manifest is written only on success (temporary file,
then rename). Any failure ends the run before the trailer, so stdout never holds a complete RVS1
stream for a failed backup. The trailer and the manifest name the producer `vmastream dev`, as
every stored image so far does, so the stored bytes stay the same.

Memory: `--ram-mib` caps the reorder buffer: buffered clusters, patch hash slots (32 bytes each)
and the zero-cluster bitmaps (1 bit per 64 KiB cluster). It must cover the bitmaps plus a
256 KiB margin. The RVS1 writer's open data frame (up to 64 MiB) and the Go runtime come on top,
so plan for about `ram-mib + 64 MiB + overhead`. When the cap would be exceeded, the farthest
early cluster is written ahead as a patch and only its hash stays in RAM. Only when hash slots
and bitmaps use up the cap does the backup stop with exit 7: raise `--ram-mib`, or lower
`MAX_WORKERS` in `/etc/restic-pve.conf` on the host.

`--view-dir DIR` writes the block view's state file (`live`, `vm-<vmid>.last`) for `watch`. It is
best effort and never affects the stream; without it there is no view. The file format is in
[house-style.md](house-style.md#the-state-file-format-version-1).

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
| qemu-img `Formatting '...'`, PVE notification failures | info; never forwarded |

Lines without a vzdump level prefix come from ssh or the host side and count as errors, except
the known informational lines above. Host exit 73 (fleecing storage below its free-space limit)
is a non-transient source failure (exit 6).

```sh
restic-pve stream backup --ssh-config ./ssh_config --ssh-host pve.example --vmid 100 \
  --manifest-out ./vm-100.manifest.json > vm-100.rvs
```

## verify, to-vma, to-raw

All three read RVS1 in one pass from stdin, or from `--in FILE`. They recompute `sha256ct` for
every device and compare it with the trailer and, with `--manifest M`, with the manifest
(devices, sizes, digests). A digest mismatch or a malformed stream exits 3, a manifest mismatch
exits 8, a manifest file that cannot be opened exits 7. Each patched position must be patched
only once, and its placeholder must be zero.

- `verify` writes nothing but the stderr result.
- `to-vma` writes a VMA stream with a fresh uuid, the current time, the config blobs verbatim and
  every cluster of every device exactly once. The final extent is written only after every
  digest matched; on any failure the output is a truncated VMA stream that `qmrestore` rejects.
  `--out FILE` must not exist. Two runs never give identical bytes (uuid, time, md5); compare the
  device digests.
- `to-raw --dir D` writes `D/<device>.raw` per device as sparse files and then `D/SHA256SUMS`
  (`sha256sum -c` checks it). Existing files are never overwritten (exit 7). On any failure every
  raw file is renamed to `*.raw.invalid` and no `SHA256SUMS` is written.

```sh
restic-pve stream verify --manifest ./vm-100.manifest.json < vm-100.rvs
restic-pve stream to-vma --in vm-100.rvs --manifest ./vm-100.manifest.json --out vm-100.vma
restic-pve stream to-raw --in vm-100.rvs --dir ./restore/example
```

### With `--pieces`: a snapshot in the piece layout

With `--pieces` the input is the tar that `restic dump --archive tar <snapshot> <root>` writes
for a snapshot in the [piece layout](#the-piece-layout-schema-2); the output and the rules above
stay the same. The reader takes the first file as the manifest (it must be `_manifest.json`; the
folder above it is the root, with or without a leading `/`), then expects every other file of
the manifest exactly once and in the layout's name order; folder entries are skipped. Each piece
must have its manifest digest and each disk its `sha256pt`; each config file its digest. Only
one piece (4 MiB) is held in memory, and nothing of the image is written to disk except
`to-raw`'s output.

- `to-vma` writes the config files as blobs. When `qemu-server.conf` has no `#qmdump#map:`
  line (a config from a Proxmox provider job), the lines the manifest kept from the last full
  read (`qmdump_map`) are put in front of it, where vzdump writes them.
- `--manifest M` is then a manifest of schema 2 (the copy the control code keeps) that the
  snapshot's own manifest must match: the same disks with the same ids, names, sizes and
  `sha256pt`.
- Exit 3: a truncated tar, a file out of order, missing, twice or extra, an entry that is not a
  plain file or a folder, a wrong piece or config file, a `sha256pt` that does not match. Exit 8:
  the manifest inside is invalid, or does not match `--manifest`. Exit 7: the input cannot be
  read, or the output written.

```sh
restic dump --archive tar <snapshot> /state/pieces/web/vm-100 | restic-pve stream verify --pieces
restic dump --archive tar <snapshot> /state/pieces/web/vm-100 | restic-pve stream to-vma --pieces --out vm-100.vma
```

## Exit codes and stderr

| Code | Meaning |
|------|---------|
| 0 | ok |
| 2 | usage (unknown command, bad flags or arguments) |
| 3 | integrity (VMA md5/uuid/reserved/blockinfo error, digest mismatch, malformed RVS1 or pieces tar) |
| 4 | forbidden content (memory state or unknown device) |
| 5 | fleecing not active (reported by vzdump) |
| 6 | source failed (ssh/vzdump non-zero exit, truncated stream) |
| 7 | local I/O (input/output files, stdout, output folder), RAM cap used up |
| 8 | manifest mismatch or missing device |
| 9 | internal error (panic or contract violation; never transient) |
| 10 | guest filesystem error during freeze (immediate stop; never transient) |

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

## The piece layout (schema 2)

`restic-pve pieces --job` (mode `auto` or `full-check`, see [commands.md](commands.md#pieces))
stores a guest that reads only its changed blocks in this layout instead of one RVS1 file. The
code is `internal/pieces` and `internal/pieces/run`; `stream verify|to-vma|to-raw --pieces` read it
(above).

A snapshot of the layout is one directory, and every file in it is part of the image:

```
_manifest.json                    the manifest, first in name order
config/qemu-server.conf           the guest config, verbatim
config/qemu-server.fw             the firewall config, when there is one
disks/<device>/<group>/<index>    the disk in pieces of 4 MiB (4194304 bytes)
```

- `<group>` is the index divided by 256, four digits; `<index>` is eight digits, both
  zero-padded: piece 31999 of `drive-scsi0` is `disks/drive-scsi0/0124/00031999`. A disk has at
  most 10,000 groups (2,560,000 pieces, about 9.8 TiB).
- Every piece is 4 MiB except the last of a disk, which holds the rest. 4 MiB is the size of
  Proxmox's change record, so a changed piece can always be read whole from the host.
- Device names are 1 to 64 characters from `A-Z a-z 0-9 . _ -` and do not start with a dot.
- restic stores and dumps a directory sorted by name, byte by byte, one path component at a time
  (so `drive-a/` comes before `drive-a-x/`). `restic dump --archive tar` therefore gives the
  manifest first, then the config files, then each disk's pieces in order; a test with
  restic 0.19.1 checks this.

`_manifest.json` (canonical JSON, at most 64 MiB; a 125 GiB disk gives about 2.1 MB):

```json
{"schema":2,"layout":"pieces","producer":"restic-pve vX.Y.Z","vmid":100,"alias":"web",
 "created":"2026-11-20T10:00:00Z","kind":"changes","parent":"<64 hex>","piece_size":4194304,
 "devices":[{"id":1,"name":"drive-scsi0","size":21474836480,"pieces":5120,
             "sha256pt":"<64 hex>","digests":["<64 hex>","..."]}],
 "config":[{"name":"qemu-server.conf","sha256":"<64 hex>"}],
 "qmdump_map":["#qmdump#map:scsi0:drive-scsi0:local:raw:"],
 "consistency":{"value":"application","detail":""},
 "record":{"job":"<32 hex>","disks":[{"name":"drive-scsi0","state":"reuse","dirty_bytes":4194304}]}}
```

| Field | Rule |
|---|---|
| `kind` | `changes`, `full` or `check`; `changes` and `check` need a `parent` (a restic snapshot id), `full` may have `null` |
| `devices` | ids 1 to 254, ascending; `pieces` equals the size divided by 4 MiB, rounded up; one digest per piece |
| `digests[i]` | SHA-256 of piece i, at its real length, 64 lowercase hex |
| `sha256pt` | SHA-256 of `"RVS2-PT\x00"`, u64le(size), u64le(4194304), then the 32 raw bytes of each digest in order |
| `config` | 1 to 16 files, `qemu-server.conf` among them |
| `qmdump_map` | at most 256 lines starting `#qmdump#map:` (the lines `qmrestore` needs, kept from the last full read) |
| `record` | the provider job of the run; required for `changes`, where every disk is `reuse`; `null` for a plain full read |

`sha256pt` binds the size, the piece size and every piece. A full read and a changed-blocks run
of the same disk content give the same value: an unchanged piece adds the parent's digest, a
read piece the digest of its bytes. A manifest is refused (exit 8) when a field is unknown or
out of its rule, when anything follows the object, when it is truncated, and when a digest list
does not give its `sha256pt`; one changed byte in any piece changes `sha256pt`.

### How a run stores the layout

The stored runs of `pieces --job` (the code is `internal/pieces`: `placeholders.go`,
`writer.go`, `restic.go`, `checks.go`; the runs in `internal/pieces/run/run.go`). restic always
runs as
`nice -n 19 ionice -c 3 restic --retry-lock 30m --json ...` with the environment the control
code passes, and its output is always read to the end (a restic stopped by a closed pipe leaves
its lock behind).

1. The parent is usable only when it is the record's base, its paths are exactly the tree root,
   its host and tags are the unit's, and its `_manifest.json` is valid, of this guest, and lists
   exactly the files of `restic ls` (each piece at its size). The tree is deleted and rebuilt
   from `restic ls --json --recursive`: one sparse file per piece, size and mtime (with
   nanoseconds) as listed. With `--parent` and `--ignore-inode` restic takes every placeholder
   from the parent without opening it. A changed-blocks run builds only on the record's base;
   a full read takes the newest pieces snapshot as its parent when the other rules hold (it
   reads every piece anyway and compares); after failed checks, and in a full check, it has no
   parent: every piece then goes through restic, which hashes it again and uploads only what
   the repository does not hold, so nothing of the base is taken on trust. The manifest of a
   full check still names the base as its `parent`.
2. A piece whose SHA-256 equals the parent's stays a placeholder and never reaches restic.
   Every other piece is staged outside the tree (zero 64 KiB clusters as holes) with the run's
   mtime, `max(now, newest parent mtime + 1 s)` in whole seconds, so it differs from its
   placeholder even when the clock was set back.
3. Kept pieces go into the tree in batches of `min(1 GiB, free / 4)` (at least 576 MiB free on
   the state file system). Each batch is `restic backup -v --host <h> --tag
   kind=guest-pieces-partial,<unit>,<vmid> --ignore-inode --parent <previous> <root>`; a run
   without a parent passes `--force` instead, so restic never picks a parent itself. After a
   batch passed C1 and C2 its pieces are punched back into placeholders. While restic runs, at
   most two batches wait in staging; then the stream is not read until a batch ends.
4. C1: every piece of the batch is `new` or `modified`, every other piece `unchanged`, each file
   of the tree reported once. C2: exit 0, no error line, the counts match. C3: `restic diff`
   of the original parent and the final snapshot changes the content only of pieces the run
   kept, adds or removes pieces only on a disk whose size changed, and touches nothing else but
   the manifest and the config. Then `restic tag --set` with the guest tags and a read of the new
   snapshot's tree; the ack follows. The run's partial snapshots are forgotten only when the run
   ends, after the ack, so the host's job and its fleecing image never wait for a repository
   lock. A failed check forgets every snapshot of the run. A failure before the final batch
   (the stream broke, a full check found a difference) runs no final batch.

## The changes stream

`restic-pve request changes` writes it on stdout for one Proxmox backup provider job, and
`restic-pve pieces` on the backup machine answers with one ack line on stdin: `probe` after a
probe, `ok <id>` after a changed-blocks run was stored and accepted (the record state is written
first), `arm` when the job starts the record before a full read, `fail` when it gives up. A
reader that gives up while pieces are still on their way sends `fail` and ends the ssh session;
the host side then sees the end of its stdin. The code is `internal/stream/changes`.

Frames use the RVS1 frame header (64 bytes, magic `RVSF`, CRC-32C of the header) with their own
type bytes:

| Type | Header fields | Payload (at most) |
|---|---|---|
| `C` header | dev 0, offset 0 | JSON `{"format":"RVC1","token","vmid","mode","pve","prev","devices":[{"id","name","size","bitmap","bitmap_name"}]}` (64 KiB) |
| `G` config | dev 0, offset 0 | u16le name length, name, data: `qemu-server.conf`, then `qemu-server.fw` when present (a VMA blob, 65535 bytes) |
| `M` map | dev = device id | state byte: 0 no record, 1 record; for 1 a bitset, 1 bit per piece, bit i in byte i/8, lowest bit first, unused bits 0 |
| `P` piece | dev = device id, offset = piece start | the piece: 4 MiB, or the rest of the disk |
| `E` end | dev 0, offset 0 | JSON `{"pieces_sent","bytes_sent"}` (4 KiB) |

- `token` is the job's 128-bit token in 32 lowercase hex; `mode` is `probe`, `use` or `new`;
  `bitmap` is what Proxmox reports per disk: `none`, `new` or `reuse`.
- `prev` is the token of the guest's last provider job on this host that ended with a success
  ack (`ok`, `arm` or `probe`), `""` when there was none since the host started. Every such job
  moves Proxmox's record of changes on, whoever started it: another unit for the same guest, a
  second backup machine, a probe. The backup machine reads only changes when `prev` is the job
  its record state names, and otherwise arms and reads in full. A header without `prev` comes
  from a host release before it; the backup machine then reads in full and says to update the
  host. A backup machine of v1.3.0 or older refuses a header with `prev` (unknown field, exit
  3): update the backup machine with the host.
- Order: the header, the config frames, then for each device in ascending id its map and, in
  mode `use`, each piece its map marks, in ascending offset; then the end frame.
- Maps: in mode `use` every map has a record when every disk is `reuse`, and none otherwise; in
  mode `probe` a disk has a record exactly when it is `reuse`; in mode `new` no disk has one.
  Pieces are sent only in mode `use`, and then every marked piece exactly once.
- The end frame's counts must equal the pieces and bytes sent. Nothing follows it; the reader
  checks that only after it sent the ack, since the host keeps stdout open until then.
- The reader checks a frame's length against the bound before it reads the payload. A
  truncated stream, a frame out of order, an offset that is not a multiple of 4 MiB, a map of the
  wrong length, an unknown field or type and a count that does not add up all end the run with
  exit 3. Piece contents are not covered by the header CRC; the backup machine checks them with
  its own digests.

The ack line, at most 1024 bytes with its newline:

```
ack <token> ok <snapshot-id>      the snapshot (64 hex) was stored, checked and accepted
ack <token> arm                   mode new, or a disk without record: the record was started
ack <token> probe                 a probe; nothing was stored
ack <token> fail <reason>         the backup machine gave up (printable ASCII, 1 to 512 bytes)
```

The host side lets the provider job succeed only on the one success word that fits the stream:
`probe` for mode `probe`, `ok` for mode `use` with every disk recorded, `arm` otherwise. `fail`,
another token, another word, a malformed line and the end of stdin before a whole line all make
the job fail, so Proxmox keeps its record of every change. A success ack makes the job the
guest's `prev` before the job ends; a job that then fails still leaves a record that covers at
least everything since that job.

A probe succeeds like any job: Proxmox's record then counts from the probe. Its numbers are
what changed since the last successful job of the guest (a backup, an arming or a probe), and
the next backup reads in full once (the backup machine that probed sets its record state to
`none`; any other one sees a `prev` it does not know).

[Back to the index](../README.md)
