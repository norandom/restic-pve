// Package watch is `restic-pve watch`: the block view of the guest backup that runs now on the
// backup machine. It reads the state file that `restic-pve stream backup` writes (package blockmap) a
// few times a second and draws the packed block view of the shared house style
// (github.com/norandom/restic-pve/blockview). It only reads: quitting it never touches the
// backup.
package watch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/norandom/restic-pve/blockview"
	"github.com/norandom/restic-pve/internal/stream/blockmap"
	"github.com/norandom/restic-pve/retro"
)

// Stale is how old the file of a running backup may get before the view says it stopped
// updating; the backup writes it twice a second.
const Stale = 15 * time.Second

// Words are the texts that come from the machine, not from the code.
type Words struct {
	Title string // title bar of a notice: "restic-pve <version> · backup machine · watch"
	Start string // the command that starts a guest backup (RESTIC_PVE_START_HINT)
}

// logHint says where the cause of a stopped or failed backup is.
const logHint = "its job's log says why"

// Brand is the right part of the bottom bar while a backup runs.
const Brand = "q leaves the view, the backup goes on"

// Snapshot is what the viewer read: the live state file and, when it fits, the guest's map
// from its last good backup.
type Snapshot struct {
	Live  *blockmap.State // nil: no file
	Prior *blockmap.State // nil: none, or it does not fit the disk
	Err   error           // the live file exists but cannot be read
}

// Load reads the live file of dir and the last map of its guest.
func Load(dir string) Snapshot {
	b, err := os.ReadFile(filepath.Join(dir, blockmap.LiveFile))
	if os.IsNotExist(err) {
		return Snapshot{}
	}
	if err != nil {
		return Snapshot{Err: err}
	}
	live, err := blockmap.Decode(b)
	if err != nil {
		return Snapshot{Err: err}
	}
	s := Snapshot{Live: live}
	if b, err := os.ReadFile(filepath.Join(dir, blockmap.LastFile(live.VMID))); err == nil {
		if p, err := blockmap.Decode(b); err == nil && p.Disk == live.Disk && len(p.Map) == len(live.Map) {
			s.Prior = p
		}
	}
	return s
}

// Speed is the read speed of a backup in bytes per second, from the last few seconds of
// state files: real progress, nothing interpolated.
type Speed struct {
	samples []sample
}

type sample struct {
	at   time.Time
	read uint64
}

// Window is the time the speed is averaged over.
const Window = 5 * time.Second

// Add notes a state file; a new run starts the samples again.
func (sp *Speed) Add(s *blockmap.State) {
	if s == nil {
		return
	}
	if n := len(sp.samples); n > 0 && (s.Updated.Before(sp.samples[n-1].at) || s.Read < sp.samples[n-1].read) {
		sp.samples = sp.samples[:0]
	}
	if n := len(sp.samples); n > 0 && s.Updated.Equal(sp.samples[n-1].at) {
		return
	}
	sp.samples = append(sp.samples, sample{s.Updated, s.Read})
	for len(sp.samples) > 2 && s.Updated.Sub(sp.samples[1].at) >= Window {
		sp.samples = sp.samples[1:]
	}
}

// Rate is the speed over the window; with fewer than two samples it is the average since the
// start of s.
func (sp *Speed) Rate(s *blockmap.State) float64 {
	if n := len(sp.samples); n >= 2 {
		a, b := sp.samples[0], sp.samples[n-1]
		if d := b.at.Sub(a.at).Seconds(); d > 0 {
			return float64(b.read-a.read) / d
		}
	}
	if s != nil {
		if d := s.Updated.Sub(s.Started).Seconds(); d > 0 {
			return float64(s.Read) / d
		}
	}
	return 0
}

// Size formats bytes the way the screens do: "512 KiB", "31 MiB", "4.2 GiB".
func Size(b float64) string {
	switch {
	case b >= 1<<40:
		return fmt.Sprintf("%.1f TiB", b/(1<<40))
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GiB", b/(1<<30))
	case b >= 10<<20:
		return fmt.Sprintf("%.0f MiB", b/(1<<20))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", b/(1<<20))
	default:
		return fmt.Sprintf("%.0f KiB", b/(1<<10))
	}
}

func guestName(s *blockmap.State) string {
	if s.Name != "" {
		return fmt.Sprintf("Guest %d (%s)", s.VMID, s.Name)
	}
	return fmt.Sprintf("Guest %d", s.VMID)
}

func percent(s *blockmap.State) int {
	if s.Disk == 0 {
		return 0
	}
	return int(s.Read * 100 / s.Disk)
}

func elapsed(s *blockmap.State, now time.Time) time.Duration {
	if s.Status != blockmap.Running {
		now = s.Updated
	}
	return max(now.Sub(s.Started), 0)
}

// Input turns a state file into the input of the packed model; prior may be nil.
func Input(s, prior *blockmap.State) blockview.Input {
	in := blockview.Input{Cells: make([]blockview.Cell, len(s.Map))}
	for i, c := range s.Map {
		in.Cells[i] = map[byte]blockview.Cell{blockmap.CellUnread: blockview.Unread, blockmap.CellEmpty: blockview.Empty,
			blockmap.CellData: blockview.Data, blockmap.CellHanded: blockview.Handed, blockmap.CellKept: blockview.Kept}[c]
	}
	if prior != nil && len(prior.Map) == len(s.Map) {
		in.Prior = make([]bool, len(s.Map))
		for i, c := range prior.Map {
			in.Prior[i] = c == blockmap.CellData || c == blockmap.CellHanded
		}
	}
	if s.Status == blockmap.Running {
		in.Reading = s.Reading
		if s.Writing {
			in.Writing = 1
		}
	}
	return in
}

// Frame is what one screen shows: the block view of a backup, or a notice.
type Frame struct {
	Screen *blockview.Screen
	Notice *blockview.Notice
	Local  *LocalScreen
}

// Draw draws the frame.
func (f Frame) Draw(screen tcell.Screen, t retro.Theme) {
	if f.Screen != nil {
		f.Screen.Draw(screen, t)
	} else if f.Local != nil {
		f.Local.Draw(screen, t)
	} else if f.Notice != nil {
		f.Notice.Draw(screen, t)
	}
}

func (w Words) notice(heading string, msg string, lines ...string) Frame {
	return Frame{Notice: &blockview.Notice{Title: w.Title, Key: "q=Leave view",
		Heading: heading, Lines: lines, Msg: msg, Brand: "restic-pve"}}
}

// Build makes the frame for a screen of cols x rows. watched is true when this viewer saw the
// run while it was running: a finished run it saw is shown with its end state, one that ended
// before the viewer started is reported in a sentence.
func Build(snap Snapshot, sp *Speed, watched bool, now time.Time, cols, rows int, w Words) Frame {
	s := snap.Live
	switch {
	case snap.Err != nil:
		return w.notice("Cannot read the backup's state", "Waiting...",
			"The state file of the guest backup cannot be read: "+snap.Err.Error()+".",
			"The view tries again every second. The backup itself is not affected.")
	case s == nil:
		return w.notice("No backup running", "Waiting for a backup...",
			"No backup is running on this machine right now.",
			"Start one with: "+w.Start+".",
			"This view stays open and shows the next backup when it starts.")
	case s.Status == blockmap.Running && now.Sub(s.Updated) > Stale:
		return w.notice("The backup stopped reporting", "Waiting for a backup...",
			fmt.Sprintf("%s: the backup stopped updating its view at %s, without a result.", guestName(s), s.Updated.Format("15:04:05")),
			"It was probably stopped; "+logHint+".",
			"Start a new one with: "+w.Start+".")
	case s.Status != blockmap.Running && !watched:
		how := "finished: all its data went into the backup"
		if s.Status == blockmap.Failed {
			how = "failed: " + s.Message
		}
		return w.notice("No backup running", "Waiting for a backup...",
			"No backup is running on this machine right now.",
			fmt.Sprintf("The last guest backup, %s, %s (at %s, after %s).", guestName(s), how,
				s.Updated.Format("2006-01-02 15:04"), retro.Elapsed(elapsed(s, now))),
			"Start one with: "+w.Start+". This view stays open and shows it.")
	case len(s.Map) == 0:
		if s.Status == blockmap.Failed {
			return w.notice("The backup failed", "Failed. Press q to quit.",
				fmt.Sprintf("%s: the backup failed before the disk arrived: %s", guestName(s), s.Message),
				"The backup was not written; "+logHint+".")
		}
		return w.notice("Starting", "Waiting for the Proxmox host...",
			fmt.Sprintf("%s: the backup has started and waits for the Proxmox host to send the disk.", guestName(s)),
			"Elapsed: "+retro.Elapsed(elapsed(s, now))+".")
	}
	if s.Kind == blockmap.KindProbe {
		return Frame{Screen: probeScreen(s, now, cols, rows)}
	}
	in, k := blockview.Group(Input(s, snap.Prior), blockview.MapSize(cols, rows))
	cells, run := blockview.Pack(in)
	blockBytes := float64(s.Disk) / float64(len(s.Map)) * float64(k)
	f := blockview.Screen{
		Title:   guestName(s) + " · full backup · " + Size(float64(s.Disk)) + " disk",
		Key:     "q=Leave view",
		Cells:   cells,
		Block:   run,
		Percent: percent(s),
		Elapsed: elapsed(s, now),
		Mode:    "Full backup",
		Legend:  blockview.Legend,
		Footer:  fmt.Sprintf("Guest %d:  1 block = %s", s.VMID, Size(blockBytes)),
		Brand:   Brand,
	}
	speed := Size(sp.Rate(s)) + "/s"
	switch s.Status {
	case blockmap.Done:
		f.Mode = "Done in " + retro.Elapsed(f.Elapsed)
		f.Msg, f.Brand = "Finished. Press q to quit.", "restic now completes the snapshot"
	case blockmap.Failed:
		f.Mode = "Failed after " + retro.Elapsed(f.Elapsed)
		f.Msg, f.Brand = "Failed: "+s.Message, logHint
	default:
		f.Msg = "Reading...  " + speed
		if s.Writing {
			f.Msg = "Writing...  " + speed
		}
	}
	return Frame{Screen: &f}
}

// probeScreen is the block view of a probe: Proxmox's record drawn in place, the count of
// changed blocks in the Status box and, there and on the message line, that nothing is read
// or stored.
func probeScreen(s *blockmap.State, now time.Time, cols, rows int) *blockview.Screen {
	in, k := blockview.Group(Input(s, nil), blockview.MapSize(cols, rows))
	var changed, all uint64
	record := "reuse" // the guest's record: reuse only when every disk has one
	for _, r := range s.Records {
		all += r.Pieces
		if r.State == "reuse" {
			changed += r.Changed
		} else {
			changed += r.Pieces
			if record != "none" {
				record = r.State
			}
		}
	}
	pct := 0
	if all > 0 {
		pct = int(changed * 100 / all)
	}
	f := &blockview.Screen{
		Title:   guestName(s) + " · probe · record " + record + " · " + Size(float64(s.Disk)) + " disk",
		Key:     "q=Leave view",
		Cells:   blockview.Place(in),
		Label:   "To read",
		Block:   int(changed),
		Percent: pct,
		Elapsed: elapsed(s, now),
		Mode:    "Probe: nothing read or stored",
		Legend:  blockview.ProbeLegend,
		Footer:  fmt.Sprintf("Guest %d:  1 block = %s", s.VMID, Size(float64(s.Disk)/float64(len(s.Map))*float64(k))),
		Brand:   "restic-pve probe",
	}
	switch {
	case s.Status == blockmap.Running:
		f.Msg = "Asking Proxmox which blocks changed..."
		f.Brand = Brand
	case s.Status == blockmap.Failed:
		f.Mode = "Probe failed"
		f.Msg, f.Brand = "Failed: "+s.Message, logHint
	case record == "reuse":
		f.Msg = fmt.Sprintf("Probe done: %d of %d pieces (4 MiB each) changed; nothing read or stored", changed, all)
	default:
		f.Msg = "Probe done: record " + record + ", the next run reads in full; nothing read or stored"
	}
	if len(s.Records) == 0 { // Proxmox has not answered yet
		f.Title = guestName(s) + " · probe · " + Size(float64(s.Disk)) + " disk"
	}
	return f
}

// probeSummary says per disk what Proxmox reported, and that nothing was read or stored.
func probeSummary(s *blockmap.State) string {
	var parts []string
	for _, r := range s.Records {
		if r.State == "reuse" {
			parts = append(parts, fmt.Sprintf("%s record reuse, %d of %d pieces of 4 MiB changed (%s)", r.Disk, r.Changed, r.Pieces,
				Size(float64(r.Changed)*4*(1<<20))))
		} else {
			parts = append(parts, fmt.Sprintf("%s record %s: no record yet, the next run reads it in full", r.Disk, r.State))
		}
	}
	if len(parts) == 0 {
		parts = []string{"no disk reported"}
	}
	return strings.Join(parts, "; ") + "; nothing was read or stored"
}

// Line is the state as one plain line, for a run without a terminal.
func Line(snap Snapshot, now time.Time, start string) string {
	s := snap.Live
	if s != nil && s.Kind == blockmap.KindProbe && snap.Err == nil && !(s.Status == blockmap.Running && now.Sub(s.Updated) > Stale) {
		switch s.Status {
		case blockmap.Running:
			return fmt.Sprintf("probe of %s: asking Proxmox which blocks changed; nothing is read or stored", lowerGuest(s))
		case blockmap.Failed:
			return fmt.Sprintf("probe of %s failed: %s", lowerGuest(s), s.Message)
		}
		return fmt.Sprintf("probe of %s: %s", lowerGuest(s), probeSummary(s))
	}
	switch {
	case snap.Err != nil:
		return "cannot read the guest backup's state: " + snap.Err.Error()
	case s == nil:
		return "no backup is running; start one with " + start
	case s.Status == blockmap.Running && now.Sub(s.Updated) > Stale:
		return fmt.Sprintf("no guest backup is running; %s stopped reporting at %s without a result ("+logHint+")",
			lowerGuest(s), s.Updated.Format("15:04:05"))
	case s.Status == blockmap.Done:
		return fmt.Sprintf("no guest backup is running; the last one, %s, finished at %s after %s", lowerGuest(s),
			s.Updated.Format("2006-01-02 15:04"), retro.Elapsed(elapsed(s, now)))
	case s.Status == blockmap.Failed:
		return fmt.Sprintf("no guest backup is running; the last one, %s, failed at %s: %s", lowerGuest(s),
			s.Updated.Format("2006-01-02 15:04"), s.Message)
	}
	var sp Speed
	return fmt.Sprintf("%s: reading %d%% %s/%s, %s/s on average, %s of data in the backup stream, elapsed %s",
		lowerGuest(s), percent(s), Size(float64(s.Read)), Size(float64(s.Disk)), Size(sp.Rate(s)),
		Size(float64(s.Handed)), retro.Elapsed(elapsed(s, now)))
}

func lowerGuest(s *blockmap.State) string {
	if s.Name != "" {
		return fmt.Sprintf("guest %d (%s)", s.VMID, s.Name)
	}
	return fmt.Sprintf("guest %d", s.VMID)
}

// LineAll is the plain line of watch: the guest backup that runs, else the local one, else
// what ran last.
func LineAll(snap Snapshot, lc Local, sp *LocalSpeed, now time.Time, start string) string {
	g := snap.Live
	guestRunning := g != nil && g.Status == blockmap.Running && now.Sub(g.Updated) <= Stale
	switch {
	case guestRunning || snap.Err != nil:
		return Line(snap, now, start)
	case lc.Running:
		return LocalLine(lc, sp, now)
	case lc.S != nil:
		return Line(snap, now, start) + "; " + localEnd(lc)
	}
	return Line(snap, now, start)
}
