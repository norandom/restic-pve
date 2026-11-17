# Setting up restic-pve on a Proxmox VE host

This page is for an admin who sets up `restic-pve` on a Proxmox VE host for the first time. It
takes about five minutes.

## What it does

`restic-pve` lets one backup machine copy your virtual machines off this Proxmox VE host. The
backup machine logs in over SSH with one key, and that key can do only one thing: ask
`restic-pve` for a copy of a guest you allowed. A running guest keeps running while it is copied.
The tool checks itself for updates once a day and installs only releases that are signed.

## What it does not do

It does not store backups, keep old versions or restore anything; the backup machine does that. It
does not give the backup machine a shell, root access or access to guests you did not tick. It does
not change your guests, storages or Proxmox settings.

## What ends up on the host

- the program `/usr/local/sbin/restic-pve`
- one user, `restic-backup`, whose password is locked; it can only log in with the key
- one key entry in `/home/restic-backup/.ssh/authorized_keys`, accepted only from the backup machine
- one sudoers file, `/etc/sudoers.d/restic-pve`, that lets this user run `restic-pve request` and
  nothing else
- one settings file, `/etc/restic-pve.conf`
- the daily update timer, `restic-pve-update.timer`, with its service

`restic-pve uninstall` removes all of it.

## Before you start

You need two values from the backup machine. Run `./setup.sh --proxmox` there; it prints both:

1. the **address** of the backup machine, for example `192.0.2.10`
2. its **public key**, one line that starts with `ssh-ed25519`, for example
   `ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI...`. Copy the whole line. It is not a secret.

You also need a root shell on the Proxmox VE host.

## Install

Paste this one line into the root shell on the Proxmox VE host:

```
curl -fsSLo /usr/local/sbin/restic-pve https://github.com/norandom/restic-pve/releases/latest/download/restic-pve-linux-amd64 && chmod 755 /usr/local/sbin/restic-pve && restic-pve install
```

A form opens. It has six screens. On every screen:

- **Enter** goes on (when the value is valid; if not, the reason shows at the bottom and you stay)
- **Esc** (or Shift-Tab) goes one screen back; what you entered is kept
- **Ctrl-C** cancels; nothing is changed

Grey text in an empty field is only an example. It is not used as a value.

### 1. Backup machine address

```
restic-pve install · step 1 of 5: Backup machine address

Type the IP address of the backup machine: the machine that runs the backups and
keeps them. Only this address may log in with the backup key. Run ./setup.sh
--proxmox on the backup machine; it prints this address.

  > 192.0.2.10
```

Type the address that `./setup.sh --proxmox` printed. Backspace deletes, Ctrl-U clears the field.

### 2. Public key

```
restic-pve install · step 2 of 5: Public key

Paste the whole public key line of the backup machine. It starts with ssh-ed25519
and is printed by ./setup.sh --proxmox on the backup machine. A public key is not
a secret.

  > ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI...
```

Paste the whole line, including `ssh-ed25519`. If the line you copied ends with a line break,
pasting it also presses Enter. A long key shows only its end; that is fine.

### 3. Guests

```
restic-pve install · step 3 of 5: Guests

Tick the virtual machines to back up. The backup machine can read only the ticked
ones.

        NUMBER     NAME     STATUS

  > [x] 100        web      running
    [ ] 101        db       stopped

  1 ticked
```

Move with the arrow keys (or j and k), tick or untick with Space, then press Enter. Tick at
least one. A long list scrolls.

### 4. Fleecing storage

```
restic-pve install · step 4 of 5: Fleecing storage

Choose where this host keeps temporary data while a running guest is read, so
the guest is not slowed down (fleecing). The usual choice is the one with the
most free space on fast disks.

        NAME       TYPE        FREE GiB

  > (*) local      dir               77
    ( ) local-lvm  lvmthin          819
```

Move to the storage and press Enter.

### 5. Limits

```
restic-pve install · step 5 of 5: Limits

These limits protect this host while a backup runs. The current values suit
most hosts.

    Start a backup only with       32 GiB
    Stop a running backup below    8 GiB
    Read limit                     40 MiB/s
    Backups at the same time       1

  > Keep these (recommended)
    Change
```

Press Enter to keep them. "Change" shows the four values as fields with their allowed ranges;
move between them with the arrow keys and press Enter after each.

### 6. Summary

```
restic-pve install · summary

Install writes these values, a backup user with its key, a sudoers file, the
binary and a daily update timer. Nothing changes before you choose Install.

  Backup machine         192.0.2.10
  Public key          ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI...
  Guests              100 (web)
  Fleecing storage    local
  Limits              start with 32 GiB free
                      stop below 8 GiB free
                      read 40 MiB/s, 1 at a time
  Backup user         restic-backup

  [ Install ]  [ Back ]  [ Cancel ]
```

Check the values. **Install** is selected; press Enter to install. Left and Right (or Tab) move
to Back or Cancel.

## What success looks like

The form closes and the host prints one line per file, then:

```
written   /etc/restic-pve.conf
...
restic-pve v1.2.3 ready: user restic-backup, key accepted from 192.0.2.10, guests: 100
This host updates restic-pve daily to the latest signed release (restic-pve-update.timer).
```

Now tell the backup machine to try it (see its own instructions).

## Later

- **Change a value** (another guest, a new key, other limits): run `restic-pve install` again.
  The form starts with the current values filled in; change what you need and press Enter on the
  rest.
- **Update**: automatic, once a day. To update now: `restic-pve update`.
- **Remove**: `restic-pve uninstall`.

## If something goes wrong

- **"this is not a Proxmox VE host"**: run it on the Proxmox VE host itself, as root, not inside
  a guest.
- **No form, only questions, or the form looks broken**: the form needs a real terminal of at
  least 60 by 20 characters. Without one the tool asks the same questions line by line. To get
  the line-by-line questions on purpose, run `restic-pve install --plain`; it shows an example
  with each question and asks `Install with these values? [y/N]` at the end, where only `y`
  installs.
- **The key is rejected**: copy the whole line again, starting with `ssh-ed25519`. Keys of
  other types (for example `ssh-rsa`) are not accepted.
- **A guest is missing from the list**: the list comes from `qm list` on this host. A guest on
  another node of a cluster must be set up on that node.
- **A storage is missing from the list**: only active storages from `pvesm status` are shown.
  Check `pvesm status`; an inactive storage must be fixed in Proxmox first.
- **The daily update failed**: `systemctl status restic-pve-update.service` shows the reason.
  A failed update leaves the installed program as it was.
