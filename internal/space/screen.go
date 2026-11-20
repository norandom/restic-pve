package space

import (
	"context"
	"fmt"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/norandom/restic-pve/internal/facts"
	"github.com/norandom/restic-pve/internal/tui"
	"github.com/norandom/restic-pve/retro"
)

// Screen is the space view.
type Screen struct {
	Title   string
	Model   Model
	Back    string
	Checked string // "checked 12 min ago", or what is being read
	Details bool
}

func (r Row) line() string {
	size := r.Size
	if size == "" {
		size = "not recorded"
	}
	when := ""
	if !r.When.IsZero() {
		when = r.When.Local().Format("Mon 2 Jan")
	}
	return fmt.Sprintf("%-11s %-6s %-13s %-10s %s", r.Name, r.Kind, size, when, r.Keep)
}

// Draw draws the view; a terminal below 60 x 20 gets the plain lines.
func (sc *Screen) Draw(s tcell.Screen, t retro.Theme) {
	cols, rows := s.Size()
	t.Background(s)
	if tui.Small(cols, rows) {
		for i, l := range tui.Rows(sc.Model.Plain(), cols) {
			if i < rows {
				retro.Put(s, 0, i, cols, t.Text(l), t.Base)
			}
		}
		s.HideCursor()
		return
	}
	m := sc.Model
	t.TitleBar(s, 0, cols, sc.Title, sc.Checked)
	y := 1
	box := func(title string, h int) (x, w, top int) {
		b := t.Box(title)
		b.SetRect(1, y, cols-2, h+2)
		b.Draw(s)
		x, top, w, _ = b.GetInnerRect()
		y += h + 2
		return x + 1, w - 2, top
	}
	storage := m.StorageLines()[:2]
	if wl := m.WorkLine(); wl != "" {
		storage = append(storage, retro.Wrap(t.Text(wl), cols-6)...)
	}
	x, w, top := box("Storage", len(storage))
	for i, l := range storage {
		retro.Put(s, x, top+i, w, t.Text(l), t.Base)
	}
	wl := m.WarningLines()
	warn := append(retro.Wrap(t.Text(wl[0]), cols-6), retro.Wrap(t.Text(wl[1]), cols-6)...)
	dont := retro.Wrap(t.Text(DontTouch), cols-2)
	n := max(min(len(m.Rows), rows-1-(len(storage)+2)-2-(len(warn)+2)-len(dont)-1), 1)
	x, w, top = box("Backups and how long they are kept", n)
	shown, more := m.Rows, 0
	if len(shown) > n {
		shown, more = shown[:n-1], len(shown)-(n-1)
	}
	for i, r := range shown {
		retro.Put(s, x, top+i, w, t.Text(r.line()), t.Base)
	}
	if more > 0 {
		retro.Put(s, x, top+len(shown), w, t.Text(fmt.Sprintf("… %d more: press Enter", more)), t.Title)
	}
	if len(m.Rows) == 0 {
		retro.Put(s, x, top, w, "backup.toml lists no source and no guest yet.", t.Base)
	}
	x, w, top = box("Warning", len(warn))
	nRule := len(retro.Wrap(t.Text(wl[0]), cols-6))
	for i, l := range warn {
		st := t.Base
		if i >= nRule {
			st = t.Strong
			if m.State == facts.SpaceLow {
				st = t.Error
			}
		}
		retro.Put(s, x, top+i, w, l, st)
	}
	for i, l := range dont {
		retro.Put(s, 1, rows-1-len(dont)+i, cols-2, l, t.Title)
	}
	t.BottomBar(s, rows-1, cols, " Enter all rows  r Refresh  q "+sc.Back, t.Bar, "restic-pve")
	if sc.Details {
		var lines []string
		for _, r := range m.Rows {
			lines = append(lines, r.line())
		}
		tui.Overlay(s, t, "Backups and how long they are kept", lines, "Enter or Esc closes this list", false)
	}
	s.HideCursor()
}

// Options are what the space view needs from the command layer.
type Options struct {
	Screen tcell.Screen
	Color  bool
	Title  string
	Back   string
	// Read gives the model and the "checked ..." text; ctx ends a storage read early, progress
	// (nil: not wanted) is told how many repositories answered.
	Read    func(ctx context.Context, refresh, cacheOnly bool, progress func(m Model, done, total int)) (Model, string)
	Stamp   func() string
	Now     func() time.Time
	Poll    time.Duration
	Idle    time.Duration
	Ready   func()
	Initial Model
}

// Run shows the space view until q, Esc or Ctrl-C (tui.ErrIdle after ten idle minutes). It
// reads the storage in the background when its cache asks for it, and on r.
func Run(o Options) error {
	t := retro.NewTheme(o.Screen, o.Color)
	sc := &Screen{Title: o.Title, Model: o.Initial, Back: o.Back, Checked: "reading…"}
	// the read runs in a goroutine and hands each step back with Post, so sc is touched only
	// on the loop; leaving stops it and waits until its children are gone
	reading := false
	var stop context.CancelFunc = func() {}
	finished := make(chan struct{})
	close(finished)
	read := func(refresh bool) {
		reading = true
		ctx, cancel := context.WithCancel(context.Background())
		mine := make(chan struct{})
		stop, finished = cancel, mine
		progress := func(m Model, done, total int) {
			tui.Post(o.Screen, func() {
				if finished == mine && reading && total > 0 {
					sc.Model, sc.Checked = m, fmt.Sprintf("reading the storage %d of %d", done, total)
				}
			})
		}
		go func() {
			defer close(mine)
			m, checked := o.Read(ctx, refresh, false, progress)
			if ctx.Err() != nil {
				return
			}
			tui.Post(o.Screen, func() {
				if finished == mine {
					sc.Model, sc.Checked, reading = m, checked, false
				}
			})
		}()
	}
	defer func() {
		stop()
		<-finished
	}()
	read(false)
	return tui.Loop{
		Screen: o.Screen, Theme: t, Poll: o.Poll, Idle: o.Idle, Now: o.Now, Ready: o.Ready,
		Draw: func(s tcell.Screen) { sc.Draw(s, t) },
		Leave: func(ev *tcell.EventKey) bool {
			return !sc.Details && (ev.Key() == tcell.KeyEscape || ev.Key() == tcell.KeyRune && (ev.Rune() == 'q' || ev.Rune() == 'Q'))
		},
		Key: func(ev *tcell.EventKey) bool {
			switch {
			case sc.Details:
				if ev.Key() == tcell.KeyEnter || ev.Key() == tcell.KeyEscape {
					sc.Details = false
					return true
				}
			case ev.Key() == tcell.KeyEnter:
				sc.Details = true
				return true
			case ev.Key() == tcell.KeyRune && (ev.Rune() == 'r' || ev.Rune() == 'R') && !reading:
				sc.Checked = "reading the storage…"
				read(true)
				return true
			}
			return false
		},
		Stamp: o.Stamp,
		Changed: func() {
			if !reading {
				sc.Model, sc.Checked = o.Read(context.Background(), false, true, nil)
			}
		},
	}.Run()
}
