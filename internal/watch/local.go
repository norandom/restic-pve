package watch

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/norandom/restic-pve/internal/facts"
	"github.com/norandom/restic-pve/retro"
)

// Local is the run of the local sources as status.json of the state root tells it.
type Local struct {
	S       *facts.LocalStatus // nil: no run yet
	Running bool               // the run lock is held
}

// LoadLocal reads status.json of the state root and probes its run lock; state "" gives
// nothing.
func LoadLocal(state string) Local {
	if state == "" {
		return Local{}
	}
	l := Local{S: facts.ReadStatus(state)}
	if l.S != nil && l.S.RunStatus == "running" {
		l.Running = facts.Locked(filepath.Join(state, facts.RunLock))
	}
	return l
}

// LocalWindow is the time the speed of a local source is measured over; status.json gets a
// new sample about every 30 s.
const LocalWindow = 30 * time.Second

// LocalSpeed is the read speed of the current unit from the samples of status.json.
type LocalSpeed struct {
	unit    string
	samples []sample
}

// Add notes a status file; a new unit starts the samples again.
func (sp *LocalSpeed) Add(s *facts.LocalStatus) {
	if s == nil || s.Updated == nil || s.BytesDone == nil {
		return
	}
	if s.CurrentUnit != sp.unit {
		sp.unit, sp.samples = s.CurrentUnit, sp.samples[:0]
	}
	at, read := *s.Updated, uint64(max(*s.BytesDone, 0))
	if n := len(sp.samples); n > 0 && (!at.After(sp.samples[n-1].at) || read < sp.samples[n-1].read) {
		if read < sp.samples[n-1].read {
			sp.samples = sp.samples[:0]
		} else {
			return
		}
	}
	sp.samples = append(sp.samples, sample{at, read})
	for len(sp.samples) > 2 && at.Sub(sp.samples[1].at) >= LocalWindow {
		sp.samples = sp.samples[1:]
	}
}

// Rate is bytes per second over the last samples; ok is false until there are two.
func (sp *LocalSpeed) Rate() (float64, bool) {
	if n := len(sp.samples); n >= 2 {
		a, b := sp.samples[0], sp.samples[n-1]
		if d := b.at.Sub(a.at).Seconds(); d > 0 {
			return float64(b.read-a.read) / d, true
		}
	}
	return 0, false
}

// finished is the new data of the units that ended in this run, in run order.
func finished(s *facts.LocalStatus) string {
	var p []string
	for _, u := range s.Order {
		if b, ok := s.DataAdded[u]; ok {
			p = append(p, u+" +"+Size(float64(b)))
		}
	}
	return strings.Join(p, " · ")
}

func localElapsed(s *facts.LocalStatus, now time.Time) time.Duration {
	if s.Started == nil {
		return 0
	}
	end := now
	if s.RunStatus != "running" && s.Updated != nil {
		end = *s.Updated
	}
	return max(end.Sub(*s.Started), 0)
}

// LocalScreen is the full screen of a running local backup.
type LocalScreen struct {
	Title, Key string
	Heading    string
	Lines      [][2]string // label, text
	Percent    float64     // -1: not known
	Msg, Brand string
}

// Draw draws the screen: the title bar, one box with the numbers and a progress track, the
// bottom bar.
func (ls LocalScreen) Draw(screen tcell.Screen, t retro.Theme) {
	cols, rows := screen.Size()
	l := retro.NewLayout(cols, rows)
	t.Background(screen)
	t.TitleBar(screen, l.TitleBarY, cols, ls.Title, ls.Key)
	box := t.Box(ls.Heading)
	box.SetRect(1, l.BodyY, cols-2, rows-2)
	box.Draw(screen)
	x, y, w, h := box.GetInnerRect()
	x, w = x+2, w-4
	row := y + 1
	for _, ln := range ls.Lines {
		if row >= y+h {
			break
		}
		retro.Put(screen, x, row, 10, ln[0], t.Title)
		for _, part := range retro.Wrap(t.Text(ln[1]), w-10) {
			if row < y+h {
				retro.Put(screen, x+10, row, w-10, part, t.Base)
			}
			row++
		}
		if ln[0] == "Read" && ls.Percent >= 0 && row < y+h {
			t.Progress(screen, x+10, row, w-10, ls.Percent/100)
			row++
		}
	}
	t.BottomBar(screen, l.BottomBarY, cols, ls.Msg, t.BarMsg, ls.Brand)
	screen.HideCursor()
}

// BuildLocal makes the frame of a running local backup.
func BuildLocal(lc Local, sp *LocalSpeed, now time.Time, w Words) Frame {
	s := lc.S
	unit := s.CurrentUnit
	ls := LocalScreen{Title: w.Title, Key: "q=Quit", Percent: -1, Brand: Brand, Msg: "Reading..."}
	if unit == "" {
		ls.Heading = "Local backup"
		ls.Lines = append(ls.Lines, [2]string{"Source", "between two sources (step: " + orDash(s.Step) + ")"})
		ls.Msg = "Working..."
	} else {
		ls.Heading = "Backing up " + facts.Source(unit)
		src := facts.Source(unit)
		if src != unit {
			src += " (unit " + unit + ")"
		}
		ls.Lines = append(ls.Lines, [2]string{"Source", src})
		read := "not known yet"
		if s.BytesDone != nil {
			read = Size(float64(*s.BytesDone))
			if s.TotalBytes != nil && *s.TotalBytes > 0 {
				read += " of " + Size(float64(*s.TotalBytes))
			}
		}
		if s.Percent != nil {
			read += fmt.Sprintf(" (%.0f%%)", *s.Percent)
			ls.Percent = *s.Percent
		}
		ls.Lines = append(ls.Lines, [2]string{"Read", read})
	}
	ls.Lines = append(ls.Lines, [2]string{"Elapsed", retro.Elapsed(localElapsed(s, now))})
	speed := "measuring (the backup reports every 30 s)"
	if r, ok := sp.Rate(); ok {
		speed = Size(r) + "/s over the last 30 s"
	}
	ls.Lines = append(ls.Lines, [2]string{"Speed", speed})
	if unit != "" {
		ls.Lines = append(ls.Lines, [2]string{"New data", "shown when this source ends"})
	}
	if f := finished(s); f != "" {
		ls.Lines = append(ls.Lines, [2]string{"Finished", "new data " + f})
	}
	if s.IOPaused {
		ls.Lines = append(ls.Lines, [2]string{"Paused", "the machine's disks are busy; the backup waits and goes on by itself"})
		ls.Msg = "Paused..."
	}
	return Frame{Local: &ls}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// localEnd says how the last local run ended, for a notice and the plain line.
func localEnd(lc Local) string {
	s := lc.S
	when := "at an unknown time"
	if s.Updated != nil {
		when = "at " + s.Updated.Local().Format("2006-01-02 15:04")
	}
	if s.RunStatus == "running" { // the file says running, the lock is free
		return "the last local run stopped " + when + " without a result (" + logHint + ")"
	}
	end := fmt.Sprintf("the last local run ended %s (%s)", when, s.RunStatus)
	if f := finished(s); f != "" {
		end += ", new data " + f
	}
	return end
}

// LocalLine is a running local backup as one plain line.
func LocalLine(lc Local, sp *LocalSpeed, now time.Time) string {
	s := lc.S
	if s.CurrentUnit == "" {
		return fmt.Sprintf("local backup: between two sources (step %s), elapsed %s", orDash(s.Step), retro.Elapsed(localElapsed(s, now)))
	}
	read := "?"
	if s.BytesDone != nil {
		read = Size(float64(*s.BytesDone))
	}
	if s.TotalBytes != nil && *s.TotalBytes > 0 {
		read += "/" + Size(float64(*s.TotalBytes))
	}
	pct := ""
	if s.Percent != nil {
		pct = fmt.Sprintf(" %.0f%%", *s.Percent)
	}
	speed := "speed not measured yet"
	if r, ok := sp.Rate(); ok {
		speed = Size(r) + "/s"
	}
	return fmt.Sprintf("local backup: %s reading%s %s, %s, elapsed %s, new data shown when this source ends",
		s.CurrentUnit, pct, read, speed, retro.Elapsed(localElapsed(s, now)))
}
