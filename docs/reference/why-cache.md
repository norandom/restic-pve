# Why the stream caches blocks

For developers and curious admins: why `restic-pve stream backup` keeps blocks in memory while a
guest is backed up, how much, and what happens when that is not enough.

## The problem

The Proxmox dump (`vzdump --stdout`) does not send a guest's disk from start to end. It sends
blocks (64 KiB clusters) in the order it gets to them. With a running guest that order is partly
out of sequence, and the blocks of several disks are mixed.

restic recognises unchanged data by its bytes. If two backups of an almost unchanged guest
arrive in a different block order, they have little in common byte for byte, and restic would
store most of the image again. So the stream puts the blocks back into plain disk order before
restic sees them. Unchanged regions then give identical bytes, and restic stores them once.

## The cache

Putting blocks in order means that a block which arrives early has to wait until the blocks
before it have come. That waiting room is the cache, called the reorder buffer in the code.

- It lives in memory only. A guest image is never written to the backup machine's disk.
- Its size is capped by `--ram-mib`, 512 MiB by default. The cap covers the waiting blocks, a
  32 byte checksum slot per patch (see below) and a small map of the empty blocks.
- On top of it come the open output frame (up to 64 MiB) and the program's own memory, so plan
  for about `ram-mib + 64 MiB` plus overhead.

## When the cache is full

The stream does not stop and does not spill to disk. It takes the block that is farthest ahead
and writes it into the output at once, out of order, as a patch. Its place in the ordered stream
is filled with zeros, and only the block's checksum stays in memory. Restore, `verify` and the
restore test put every patch back in its place and check that each position is patched at most
once and was zero before.

A patch costs a little deduplication for that block, nothing more. Only when the checksum slots
and the map alone use up the cap does the backup stop with exit 7. Then raise `--ram-mib`, or
lower `MAX_WORKERS` in `/etc/restic-pve.conf` on the Proxmox host, which makes the dump send
blocks closer to disk order.

## What you see of it

- Progress lines and the final result carry `ram_bytes`, `buffer_ram_peak`, `patches` and
  `patch_count`.
- In the block view, blocks that arrived early show as data ahead of the yellow run.

## What changes with incremental reads

A changed-blocks run does not go through the dump. Proxmox offers the disk as a block device,
and the backup side chooses what to read and in which order: the changed 4 MiB pieces from front
to back. Nothing arrives early, so that path needs no cache.

Full reads keep the cache, because they still use the dump: the first run of a guest, a run
after the guest was stopped or restarted, and the regular full check.

[Back to the index](../README.md)
