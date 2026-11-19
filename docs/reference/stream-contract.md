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
| `verify` | RVS1 on stdin or `--in` | fingerprint check against trailer and `--manifest` |
| `to-vma` | RVS1 on stdin or `--in` | uncompressed VMA (`--out FILE` or `-`) |
| `to-raw` | RVS1 on stdin or `--in` | `<dir>/<device>.raw` (sparse) and `<dir>/SHA256SUMS` |

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

## Exit codes and stderr

| Code | Meaning |
|------|---------|
| 0 | ok |
| 2 | usage (unknown command, bad flags or arguments) |
| 3 | integrity (VMA md5/uuid/reserved/blockinfo error, digest mismatch, malformed RVS1) |
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

[Back to the index](../README.md)
