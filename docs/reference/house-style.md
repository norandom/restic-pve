# House style: the retro disk-tool look

For developers who draw or change a screen of `restic-pve`: the reference the look comes from,
the rules every screen follows, and where the code lives. Admins find the screens themselves in
[../screens.md](../screens.md).

Every full-screen view of this project (the install form on the Proxmox host, the status, keys
and space screens on the backup machine, the backup view, and the setup and restore screens
still to come) looks like one program: a DOS-era disk tool. A blue screen, white text, thin single-line boxes with yellow titles, a grey
bar on top and a grey message bar at the bottom, yellow as the one highlight. The backup view is
the centrepiece: a map of blocks for the whole disk, and a yellow run that grows left to right,
top to bottom, line by line.

The look is an homage in layout, characters, colours and movement. Names, logos and product
texts of the original appear nowhere on these screens. The project calls it the retro disk-tool
look.

Without a terminal, with `TERM=dumb` or on a terminal that is too small, every program falls back
to plain lines. With `NO_COLOR` the layout stays and the colours go.

## 1. The reference

The reference is the full-screen disk defragmenter that came with MS-DOS 6.x. Microsoft licensed
it from Symantec and it is a cut-down version of that vendor's own optimizer, which is why both
look alike. The work was done from pictures, not from memory.

### Sources

| | Source | What it is |
|---|---|---|
| R1 | The operator's reference picture (2026-11-17, not in this repository) | Capture of the map, Status and Legend boxes mid-run, from an emulator |
| R2 | Thumbnail of the video "DOS Defrag (ASMR)", youtube.com/watch?v=Nidwz3BzFCM | Full screen in an emulator, writing, 11 % done |
| R3 | Thumbnail of a video of the program on a real CRT, youtube.com/watch?v=lhjgVovLkXY | Photo of real hardware, 51 % done |
| R4 | Microsoft Knowledge Base Q105068, Q103555 (archived at jeffpar.github.io/kbarchive) | `X` marks unmovable blocks on the drive map |
| R5 | Microsoft Knowledge Base Q120962 | The program's copyright line names Symantec; unchanged in MS-DOS 6.22 |
| R6 | Wikipedia, "Microsoft Drive Optimizer" | The MS-DOS 6 program is a licensed, cut-down version of the Norton optimizer |
| R7 | photogabble.co.uk, "Defrag like it's 1992" | Write-up of two browser recreations: `r` reading, `W` writing, `B` bad, `X` unmovable |

R2 and R3 are video thumbnails (single frames); the videos themselves were not watched. R7 describes
recreations, not the original; it is used only where it agrees with R1 to R3.

### Confirmed (with the source)

- Layout (R1, R2, R3). Row 1: a light grey bar, the menu word at the left in black, a key
  label at the right in inverse (white on black) while the program works. Rows 2 to about 18: the
  map, edge to edge, no frame. Below it, side by side, two boxes with thin single-line white
  frames and their titles centred in the top line in yellow: Status on the left, Legend
  on the right. Last row: a light grey bar with a status word in dark red on the left
  ("Writing...", "Reading...", "Updating FAT") and, after a vertical line, the program's name in
  dark red on the right.
- Map (R1, R2, R3). One character cell per block, many blocks per cluster group
  ("1 block = 54 clusters"). Cells sit in rows left to right, rows top to bottom. The last row
  stops where the disk ends; the rest of that row is plain background (R1, R2).
- Cells (R1, R2, R3, legend text in R1 and R2). Used: a white tall cell with a small dark dot
  in its middle (it looks like the code page 437 inverse bullet `◘`). Unused: a light shaded
  cell (looks like `░`, light shade). Done blocks: the same dotted cell in yellow. Reading: `r`.
  Writing: `W`. Bad: `B`. Unmovable: `X` (R1, R2, R4). The legend lists them in two columns:
  Used / Unused, Reading / Writing, Bad / Unmovable, then "Drive C: 1 block = N clusters".
- Colours (R1, R2, R3). Blue screen, white text and frames, yellow box titles and done blocks,
  grey bars with black text, dark red text on the bottom bar.
- Status box (R1, R2, R3). Line 1: "Cluster N" left, "P%" right. Line 2: a progress track the
  width of the box: shaded when empty, solid white where done (R2, R3). Line 3: "Elapsed Time:
  hh:mm:ss", centred. Line 4: the method ("Full Optimization"), centred.
- Movement (R1, R2, R3 as stills). The yellow run starts in the top-left corner and grows
  along the rows; `W` cells stand at the end of the yellow run (R2); the rest of the map is still
  white and shaded. The operator describes the motion as "a yellow progress bar that moves left
  to right, top to bottom, line by line".
- Frames (R1, R2, R3). Thin single lines; no double lines, no shadows.

### Not confirmed, or sources disagree

- The exact blue. Emulator captures (R1, R2) show a light blue, about `#5555FF`; the CRT photo
  (R3) shows a deeper blue, nearer `#0000AA`. R1 is binding, so the palette uses `#5555FF`.
- The exact characters. The used and unused cells are judged from their shape; a manual that
  confirms the code points was not found. `◘` (CP437 0x08) and `░` (CP437 0xB0) match the pictures.
- The motion in time. Only stills were available: how fast the marker moves, whether `r` and `W`
  alternate per cell, and how the map looks between frames are not confirmed.
- The ancestor optimizer's own screen. No picture of it was found; nothing here relies on it.
- The idle key label ("F1=Help" instead of the stop key) appears only in a recreation (R7).
- The gaps between cells. In R1 every cell column is separated by a thin line of
  background. That is the ninth pixel column of the VGA text font, which stays blank for these
  characters. A modern terminal draws shades and blocks edge to edge, so this map has no gaps.

## 2. The rules

These rules hold whatever library draws the screen.

### Palette

| Role | Truecolor / 256 colours | 16 colours | 8 colours | No colour (`NO_COLOR`) |
|---|---|---|---|---|
| Screen background | `#5555FF` | 12 bright blue | 4 blue | terminal default |
| Text, frames, blocks with data | `#FFFFFF` | 15 bright white | 7 white + bold | default |
| Box titles, done blocks, focus background | `#FFFF55` | 11 bright yellow | 3 yellow + bold | bold; focus inverse |
| Title bar and bottom bar | `#AAAAAA` background, `#000000` text | 7 / 0 | 7 / 0 | inverse |
| Key label on the title bar | `#FFFFFF` on `#000000` | 15 on 0 | 7 + bold on 0 | default |
| Status word, program name on the bottom bar | `#AA0000` on `#AAAAAA` | 1 on 7 | 1 on 7 | inverse + bold |
| Text field | `#FFFFFF` on `#0000AA` | 15 on 4 | 7 + bold on 0 | underline |
| Example in an empty field | `#AAAAAA` on `#0000AA` | 7 on 4 | 7 on 0 | dim + underline |
| Error | `#FFFFFF` bold on `#AA0000` | 15 on 1 | 7 on 1 | bold |
| Focused row or button | `#5555FF` on `#FFFF55` | 12 on 11 | 4 on 3 | inverse |

With 256 colours the exact values are matched to the nearest palette entry (`#5f5fff`,
`#ffff5f`, ...). With 8 colours the screen is the dark blue of the CRT photo and bold makes the
white and the yellow bright, as on the Linux console. Colour depth comes from the terminal
description (`TERM`, `COLORTERM=truecolor`); `NO_COLOR` set to anything turns colours off.

### Characters

The look of code page 437 through Unicode; plain ASCII when the character set of the locale is
not UTF-8 (`LC_ALL`, `LC_CTYPE`, `LANG`; `C` and `POSIX` are ASCII).

| Element | UTF-8 | ASCII |
|---|---|---|
| Frame | `┌ ┐ └ ┘ ─ │` | `+ + + + - \|` |
| Separator on the bottom bar | `│` | `\|` |
| Block with data, done block (in colour) | `◘` | `o` |
| Empty block, empty part of a progress track | `░` | `.` |
| Done part of a progress track; done block without colour | `█` | `#` |
| Passed empty block without colour | `▒` | `:` |
| Block not read yet (live view) | `·` | blank |
| Pointer in front of the focused row | `►` | `>` |
| Middle dot between keys, ellipsis | `·` `…` | `-` `...` |

### Screen anatomy

```
row 1        title bar    grey: " <program> · <where you are>"          inverse " <key>=<action> "
rows 2..n-7  body         a framed box with the step's title (forms) or the map (backup view)
rows n-6..n-1 lower boxes  two framed boxes side by side, four lines inside each
row n        bottom bar   grey: keys or a status word                   "│ <program>" in red
```

- The lower boxes split the width in half, one column of margin at each edge and between them,
  like Status and Legend in the reference.
- Box titles are centred in the top frame line with one space on each side.
- Text inside a box starts two columns in (one for the frame, one blank).
- The smallest screen is 60 x 20. A smaller terminal gets plain lines.

### Forms

- Main box: titled with the step; first the explanation in two or three plain sentences,
  then a blank line, then the field, the list or the summary.
- Text field: `> ` and a navy field to the end of the line; the cursor at the end of the value.
  Empty, it shows the example in grey; the example is never taken as the value. A value longer
  than the field shows its end after `…`.
- Pick list (one of many): rows `( ) name ...` and `(*)` on the chosen one.
- Checklist: rows `[ ] name ...`, `[x]` when ticked; Space ticks.
- Focus: the focused row or button is drawn yellow with blue text from the pointer `►` to the
  end of the line; without colour in inverse video.
- Buttons: `[ Install ]  [ Back ]  [ Cancel ]`, the focused one yellow.
- Lower left box "Progress": "Step n of 5" and the share of steps done, a progress track,
  the step's name and the job, centred, like the Status box.
- Lower right box "Help": the short help for the current field (an example, the allowed range,
  the count of ticked rows). A broken rule shows here first in the error colours; the help stays
  below it when it still fits.
- Title bar: program, step and its name on the left; `Ctrl-C=Cancel` on the right.
- Bottom bar: the keys of this screen; `│ restic-pve` on the right.

### The backup view (block map)

```
 <title bar: guest, kind of backup>                                   Esc=Stop
 ◘◘◘◘◘◘◘◘◘◘◘◘◘◘◘◘◘◘◘◘◘◘r◘░░░◘░░░◘◘░░░░◘░◘░░░░◘░░◘░░░◘◘◘◘◘░░░░░◘◘░░◘░░░◘◘░░◘◘◘
 ░◘◘░░◘◘◘░░░░◘░░░◘░░◘░░░░░░░░░░░░◘░░░◘◘◘░░◘░░░░◘◘◘◘◘◘◘◘◘◘◘◘◘◘░◘░◘░░░░░◘◘░◘░░◘
 ...                                                                    (the last row stops)
 ┌────────────── Status ──────────────┐ ┌────────────── Legend ───────────────┐
 │  Block 589                     1%  │ │  ◘ - Has data     ░ - Empty         │
 │  █░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░  │ │  r - Reading      W - Writing       │
 │       Elapsed Time: 00:00:22       │ │  ◘ - In backup                      │
 │            Full backup             │ │  Guest 100:   1 block = 64 MiB      │
 └────────────────────────────────────┘ └─────────────────────────────────────┘
Reading...                                                          │ restic-pve
```

- Map: one cell per block, from the top-left, left to right, top to bottom, no frame, one
  column of margin. The block size is the disk size divided by the cells that fit, rounded up to
  whole MiB; the last row stops where the disk ends.
- Cell states (character and colour as in the tables above):

  | State | Meaning | Cell |
  |---|---|---|
  | Has data | holds data, not read yet in this run | white `◘` |
  | Empty | holds no data | white `░` |
  | Reading | being read now | white `r` |
  | Writing | new data from it goes into the backup now | white `W` |
  | In backup | read; its data is in the backup | yellow `◘` |
  | Passed, empty | the marker went past it, nothing to keep | yellow `░` |
  | Not read yet | the live view knows nothing about it yet | white `·` |

  Bad (`B`) and unmovable (`X`) have no meaning in a backup and are not used.
- Movement: there are two ways to show the progress of a backup on the map. Both grow a
  yellow run from the top-left, left to right, line by line. The view only shows what the
  backup really did; it never changes how the backup works.
  - In place. The marker (`r`, then `W` while new data is written) moves one block at a
    time, left to right, then on to the next row. Every block behind it turns yellow where it
    is (blocks with data as yellow `◘`, empty ones as yellow `░`), so the run reads like a
    progress bar. A full backup moves at an even pace and shows `W` on every block with data.
    An incremental backup races over unchanged blocks (they turn yellow without `W`) and stays
    on changed blocks, `r` and then `W`.
  - Packed. Closest to the movement of the reference: the backup keeps only the blocks that
    hold data and stores them packed, so the blocks move. The next scattered block with data
    ahead of the run shows `r`; then the cell at the end of the run shows `W`; then that cell
    turns yellow and the block's old place becomes empty, and the next block is read in the
    same step. A block that already sits at the end of the run turns yellow in place. The
    yellow run has no holes, and the map after it thins out to empty as blocks are taken. At
    the end one solid yellow run stands at the top, as long as the number of blocks with data,
    and the rest of the map is empty. A full backup starts slowly, one block at a time, and
    speeds up gradually (shorter `r` and `W`, then several blocks read and written together).
    An incremental backup lets unchanged blocks join the run quickly without `r` and `W`; only
    changed blocks are read and written.
- Status box: "Block N" and the percentage; the progress track; "Elapsed Time: hh:mm:ss"; the
  kind of run ("Full backup", "Incremental backup"). In the packed view "Block N" counts the
  blocks in the run, the percentage is the share of blocks with data in it, the kind of run
  ends in "· packed view", and at the end the last line reads "Done in hh:mm:ss" until the
  screen is closed.
- Legend box: the states in two columns as above, then "Guest <id>:   1 block = N MiB"; in
  the packed view "Data: N blocks x M MiB".
- Bottom bar: the status word in red ("Reading...", "Writing...", "Finished."), and
  `│ restic-pve` on the right.

### The live view of a real backup

The packed view is also the live view of a real guest backup on the backup machine
(`restic-pve watch` there). It is drawn by the same code as the
demo (package `blockview`), but every change on the screen comes from a real event of the
backup. Nothing is animated: when the backup waits, the picture waits.

| On screen | What really happens |
|---|---|
| `r` on a block | clusters of that part of the disk are arriving from the Proxmox host now and the part is not complete yet |
| `W` at the end of the run | bytes went into the backup stream (the pipe restic reads) since the last update, or a write is in progress |
| a block joins the yellow run | every cluster of that part arrived and all its data has been written into the backup stream |
| a white block | that part holds data that is not in the backup stream yet (read ahead and waiting in the reorder buffer, or in the frame being filled) |
| a shaded block | every cluster of that part arrived and none held data; or its data moved into the run |
| `·` | not read yet in this run, and no earlier backup says it holds data |
| speed on the bottom bar | disk bytes passed per second over the last five seconds, from the state file |

What is representation and not a fact: only the exact cell a block lands in inside the yellow
run. The backup stores the data of the blocks in disk order, without the empty parts' data, so
the run is "as many blocks as hold data", packed from the top-left. The empty parts are not read
as data: the Proxmox host sends no bytes for them; the stream to restic carries them as zeros,
which restic stores once.

The last known layout: when the guest's last good backup left its map, the blocks it found with
data are drawn white from the start, before they are read in this run; the read corrects them as
it passes.

Status box: "Block N" is the length of the yellow run, the percentage is the share of the disk
passed, then the elapsed time and "Full backup"; at the end "Done in hh:mm:ss" (all data is in
the backup stream; restic then completes its snapshot) or "Failed after hh:mm:ss" with the reason
on the bottom bar. Legend footer: "Guest <id>:  1 block = <size>". The bottom bar says that `q`
leaves the view and the backup goes on. When no backup runs, the screen says so in a sentence and
names the command that starts one.

#### The probe view

`restic-pve pieces --job FILE` with mode `probe` shows Proxmox's record of changed blocks for a
guest, as the map of the same view, drawn in place: nothing moves into a run, because nothing is
read and nothing is stored. A white block is a part the next run would read (Proxmox marks it
changed, or the disk has no record yet); a yellow-shaded block is unchanged. Title:
`Guest <id> (<alias>) · probe · record reuse|new|none · <size> disk`. Status box: "To read N"
(pieces of 4 MiB) and its share of the disk, the elapsed time, and "Probe: nothing read or
stored". Legend: "To read" and "Unchanged", and the size of one block. Bottom bar at the end:
`Probe done: 97 of 8193 pieces (4 MiB each) changed; nothing read or stored`, or for a disk
without a record `Probe done: record new, the next run reads in full; nothing read or stored`.
The view stays until `q`; it redraws only when the state file changes. Without a terminal the
plain line is the result message. Captures from a trial with stand-ins: a disk with a record
and 97 changed pieces, a first probe (`new`), and a guest whose disks have no record (`none`).

#### The state file (format version 2)

Version 2 adds one cell and three fields; `restic-pve watch` reads versions 1 and 2, and a
version-1 file is a full read.

- Cell `=`: unchanged, kept from the last backup: not read in a changed-blocks run (or read and
  equal in a full read). In the packed view it counts as in the backup and joins the yellow run.
- `kind full|changes|check|probe` after `mode`: the kind of run. `stream backup` writes `full`.
- `skipped <bytes>`: the bytes not read because unchanged.
- `records <disk>=<none|new|reuse>:<changed>/<pieces> ...`: Proxmox's record per disk, as a
  provider job reported it, with the pieces of 4 MiB it marks changed and the disk's pieces
  (at most 16 disks; empty for a full read). This field is an addition to the design's two.

A probe's final map is not kept as the guest's last map (`vm-<vmid>.last`), which stays the
layout of a real read.

#### The state file (format version 1)

`restic-pve stream backup --view-dir DIR` writes `DIR/live` at most twice a second (write to a temporary
file, then rename) and, after a complete stream, a copy as `DIR/vm-<vmid>.last`. Plain text, one
`key value` line per field, about 4.5 KB:

```
vmastream-view 1
guest 100                 guest id
name example              guest alias (may be empty)
mode full                 kind of backup
status running            running | done | failed
started 1763380000123     Unix milliseconds
updated 1763380012456     Unix milliseconds of this snapshot
disk 34359738368          bytes of all disks of the guest, laid out one after the other
read 1234567              bytes of the disks passed (clusters arrived, empty ones included)
data 1234567              bytes of clusters that held data
handed 1234567            bytes of data written into the backup stream
written 1234567           bytes written to the backup stream (framing and zero runs included)
writing 1                 a write to the stream is running or happened since the last snapshot
reading 17 18             cells with clusters arriving now
message text              why the stream failed, one line, or empty
map ??..oo##...           one character per cell, at most 4096 cells
```

Map characters: `?` not read yet (or partly read, no data so far), `.` empty (every cluster
arrived, none held data), `o` holds data not all in the stream yet (or not complete), `#` complete
and all its data in the stream. Cell `c` covers the clusters from `ceil(c*N/cells)` to the next
cell's first, `N` the clusters of all disks. A reader rejects a file whose first line is not
`vmastream-view 1` (the format keeps the name of the program that introduced it).
`restic-pve watch` reads `DIR` = `<RESTIC_PVE_STATE>/guests/view`.

The writer never affects the backup: the stream only counts into its own arrays and stores a
snapshot pointer; a separate goroutine writes the file; on the first write error it logs one
line and stays off for the run; `VMASTREAM_VIEW=off` turns it off. The viewer only reads.

### The screens of the backup machine (status, keys, space)

The same frame: title bar `restic-pve <version> · backup machine · <screen>` with the time of
the last storage read on the right, framed boxes with yellow titles, one line with the verdict or
the fixed sentence, the key bar at the bottom. They fit 80 x 24; below 60 x 20, with `TERM=dumb`,
`--plain` or without a terminal they print plain lines.

- Status: three boxes, one per question: "Is there a recent copy away from the site?" (one
  row per source instance and guest, not safe first, `■ NO` in the error colours), "Have I seen a
  restore work?" (restore test, drill, the bad-day guide), "Now and next" (running, next, space,
  updates and alerts, password proof); between the first two, when a guest reads only changes,
  "Guests that read only changes" with two lines per guest (last run, read and skipped; time and
  last full check, or the fault in the error colours), only as far as the copies leave room.
  Under them the verdict: "All copies are recent and
  checked." or the first instance that is not safe with its one action. Enter lists all rows,
  `?` shows the help.
- Keys: one box with two lines per key (name, copy away, state and where it is; what it
  does and what happens when it is lost), a box with the state of the password copy, a box with
  the release signing key note and the other secrets. `p` opens the proof box: a navy field that
  shows a fixed `●●●●●●` after the first key, whatever the length.
- Space: Storage (total, used, free as reported; the pieces working space per guest and in
  total when there is one), Backups and how long they are kept, Warning
  (the rule in words with today's numbers; the action in the error colours when it holds), and
  the fixed sentence in yellow: do not delete or move files on the storage by hand.

They redraw only when a file they read changes or the minute of "checked N min ago" turns, and
close themselves after ten minutes without a key press.

## 3. Where the screens differ from the reference, and why

- Own words. The menu bar names the program and the step, not a menu; the legend says
  "Has data", "Empty", "In backup"; the bottom bar names `restic-pve`. No product names.
- No `B` and `X`. They have no meaning in a backup.
- Passed empty blocks turn yellow (shaded), in the in-place view. In the reference, data is
  moved to the front, so the yellow run is solid. In the in-place view the empty blocks stay
  where they are; colouring them yellow keeps the run continuous, so it still reads as one
  progress bar. The packed view moves the blocks like the reference and needs no yellow shade.
- No gaps between cells. Terminals draw shade and block characters to the cell edges.
- The forms have no counterpart in the reference. They borrow its parts: grey bars, the
  blue screen, yellow box titles, the two lower boxes (Progress like Status, Help like Legend).
- Smallest size 60 x 20 instead of the fixed 80 x 25 text mode; the map fills whatever size
  the terminal has.

## 4. In this repository

`retro` (`github.com/norandom/restic-pve/retro`) holds the style: palette and characters
(`Theme`), bars, framed boxes, the progress track, the block map, the Status and Legend boxes. It
draws on `tcell` screens and builds on `tview` primitives and has no install code.

`blockview` (`github.com/norandom/restic-pve/blockview`) is the packed block view: the model
(`Input`: one `Cell` per block, not read yet / empty / data / in the backup, plus the blocks being
read, the `W` cells and the last known layout; `Pack` turns it into the map, `Group` fits a large
map to the screen) and the full screen (`Screen`, and `Notice` for a screen that only says
something). The demo and the live view on the backup machine both draw through it.

`restic-pve demo [pack] [full|incremental]` shows the block map with a simulated run of a
125 GiB disk: without `pack` in place, with `pack` packed (through `blockview`). It reads no data
and is not in the usage line.

[Back to the index](../README.md)
