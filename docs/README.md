# Documentation index

For anyone looking for the page that does one job. Each page starts with who it is for. The
project overview, its limits and the two-minute demo are in the [README](../README.md).

## For adopters

- [Is this for you?](../README.md#is-this-for-you): who it fits, who it does not, what is not
  built yet.
- [Try it in two minutes](../README.md#try-it-in-two-minutes): download the binary and run the
  demo, which reads nothing and changes nothing.
- [host.md](host.md): every file the tool puts on a Proxmox host, what the backup user can and
  cannot do, and how to remove it all.

## For admins

- [onboarding.md](onboarding.md): install on a Proxmox host, one screen at a time, with a capture
  of each step. About five minutes.
- [host.md](host.md): the host files, the settings file, the key entry and sudo rule, daily
  updates, checking a download, uninstall.
- [backup-machine.md](backup-machine.md): `status`, `keys`, `space` and `watch` on the backup
  machine, the variables they need, and what is not published yet.
- [screens.md](screens.md): every screen in one place, with the machine, the command and the keys
  that work on it.
- [troubleshooting.md](troubleshooting.md): messages the tool prints, and what to do about each.

## For the maintainer

- [release-signing.md](release-signing.md): how a release is built, approved and signed, the
  one-time key setup, checking a release by hand, and key rotation.

## For developers (reference)

- [reference/commands.md](reference/commands.md): every subcommand per role, its flags, the
  variables it reads and its exit codes, including the install, uninstall and update details.
- [reference/request-interface.md](reference/request-interface.md): `request ping|info|dump`
  between the machines, the settings file grammar, the SSH and sudo entries, exit codes.
- [reference/stream-contract.md](reference/stream-contract.md): `stream backup|verify|to-vma|to-raw`,
  flags, stdout and stderr, the result line, exit codes 0 and 2 to 10, the vzdump log table.
- [reference/release-assets.md](reference/release-assets.md): the four files of a release and the
  `relsign` commands.
- [reference/build-and-test.md](reference/build-and-test.md): the pinned Go image, vendoring, the
  check commands, the release build, the binary size, tests without Proxmox.
- [reference/house-style.md](reference/house-style.md): the retro screen look, its sources,
  palette, characters, the block view and its state file.

## Pictures

The captures in [img/](img/) were made from a terminal with made-up names and stand-in storage.

[Back to the project README](../README.md)
