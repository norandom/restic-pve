// Package tui is the event loop the backup role's screens share (status, keys, space): it
// draws once, then again only when something visible changed (a key, a resize, a watched
// file, the minute of a "N min ago" text, a finished background read); it closes the screen
// after ten minutes without a key press; q, Q, Esc and Ctrl-C leave. It never writes a file.
package tui

import (
	"errors"
	"os"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/norandom/restic-pve/retro"
)

// Defaults of a loop.
const (
	Poll = 2 * time.Second  // how often the watched files are looked at (stat only)
	Idle = 10 * time.Minute // the screen closes after this long without a key
)

// ErrIdle is returned when the screen closed itself after Idle without a key press.
var ErrIdle = errors.New("closed after ten minutes without a key press")

// Loop is one screen's event loop.
type Loop struct {
	Screen tcell.Screen
	Theme  retro.Theme
	Draw   func(s tcell.Screen)
	// Key handles a key that does not leave; it returns true when the screen changed. A key
	// handler that starts slow work runs it in a goroutine and hands the result back with
	// Post.
	Key func(ev *tcell.EventKey) (changed bool)
	// Leave decides whether a key leaves; nil: q, Q, Esc and Ctrl-C. Ctrl-C always leaves.
	Leave func(ev *tcell.EventKey) bool
	// Stamp is a cheap summary of what the screen shows from files and the clock; a new value
	// calls Changed and redraws.
	Stamp   func() string
	Changed func()
	Paste   func(start bool) // the start and the end of a bracketed paste
	Poll    time.Duration
	Idle    time.Duration
	Now     func() time.Time
	Ready   func() // tests: called after each pass of the loop that drew or decided not to
}

// Post runs fn on the loop (between events) and redraws; it is how background work hands
// its result to the screen.
func Post(s tcell.Screen, fn func()) { _ = s.PostEvent(tcell.NewEventInterrupt(fn)) }

// Run shows the screen until a leaving key (nil), the idle time (ErrIdle) or the end of the
// terminal (ErrIdle).
func (l Loop) Run() error {
	if l.Poll <= 0 {
		l.Poll = Poll
	}
	if l.Idle <= 0 {
		l.Idle = Idle
	}
	if l.Now == nil {
		l.Now = time.Now
	}
	l.Theme.Apply()
	events := make(chan tcell.Event, 16)
	quit := make(chan struct{})
	defer close(quit)
	go l.Screen.ChannelEvents(events, quit)
	tick := time.NewTicker(l.Poll)
	defer tick.Stop()
	draw := func() {
		l.Draw(l.Screen)
		l.Screen.Show()
	}
	stamp := ""
	if l.Stamp != nil {
		stamp = l.Stamp()
	}
	draw()
	lastKey := l.Now()
	ready := func() {
		if l.Ready != nil {
			l.Ready()
		}
	}
	ready()
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return ErrIdle
			}
			switch ev := ev.(type) {
			case *tcell.EventKey:
				lastKey = l.Now()
				if ev.Key() == tcell.KeyCtrlC || l.leaves(ev) {
					return nil
				}
				if l.Key != nil && l.Key(ev) {
					draw()
				}
			case *tcell.EventPaste:
				lastKey = l.Now()
				if l.Paste != nil {
					l.Paste(ev.Start())
				}
			case *tcell.EventResize:
				l.Screen.Sync()
				draw()
			case *tcell.EventInterrupt:
				if fn, ok := ev.Data().(func()); ok {
					fn()
				}
				draw()
			}
			ready()
		case <-tick.C:
			if l.Now().Sub(lastKey) >= l.Idle {
				return ErrIdle
			}
			if l.Stamp != nil {
				if s := l.Stamp(); s != stamp {
					stamp = s
					if l.Changed != nil {
						l.Changed()
					}
					draw()
				}
			}
			ready()
		}
	}
}

func (l Loop) leaves(ev *tcell.EventKey) bool {
	if l.Leave != nil {
		return l.Leave(ev)
	}
	return ev.Key() == tcell.KeyEscape || ev.Key() == tcell.KeyRune && (ev.Rune() == 'q' || ev.Rune() == 'Q')
}

// FileStamp sums up the size and mtime of paths and the files in the folders among them,
// with a stat each: the cheap test of whether a screen must read its facts again.
func FileStamp(paths ...string) string {
	b := make([]byte, 0, 256)
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			b = append(b, '-')
			continue
		}
		b = fi.ModTime().AppendFormat(b, time.RFC3339Nano)
		b = append(b, byte(fi.Size()), '|')
		if fi.IsDir() {
			if es, err := os.ReadDir(p); err == nil {
				for _, e := range es {
					if i, err := e.Info(); err == nil {
						b = append(b, e.Name()...)
						b = i.ModTime().AppendFormat(b, time.RFC3339Nano)
					}
				}
			}
		}
	}
	return string(b)
}

// Small is true for a terminal below the smallest size of the house style, 60 x 20.
func Small(cols, rows int) bool { return cols < 60 || rows < 20 }

// Overlay draws a framed box with title and paragraphs in the middle of the screen, for help
// texts over a screen. With wrap false every line is one row, cut at the frame (lists).
func Overlay(s tcell.Screen, t retro.Theme, title string, lines []string, foot string, wrap bool) {
	cols, rows := s.Size()
	w := min(cols-4, 76)
	var wrapped []string
	for _, l := range lines {
		if l == "" || !wrap {
			wrapped = append(wrapped, t.Text(l))
			continue
		}
		wrapped = append(wrapped, retro.Wrap(t.Text(l), w-4)...)
	}
	h := min(len(wrapped)+4, rows-2)
	x, y := (cols-w)/2, max((rows-h)/2, 1)
	retro.Fill(s, x, y, w, h, ' ', t.Base)
	box := t.Box(title)
	box.SetRect(x, y, w, h)
	box.Draw(s)
	for i, l := range wrapped {
		if i >= h-4 {
			break
		}
		retro.Put(s, x+2, y+1+i, w-4, l, t.Base)
	}
	if foot != "" {
		retro.Put(s, x+2, y+h-2, w-4, t.Text(foot), t.Strong)
	}
}

// Rows breaks plain lines into rows of at most cols columns, keeping their spacing: the
// screen of a terminal below the smallest size.
func Rows(lines []string, cols int) []string {
	var out []string
	for _, l := range lines {
		for retro.Width(l) > cols {
			head := retro.Cut(l, cols)
			out = append(out, head)
			l = l[len(head):]
		}
		out = append(out, l)
	}
	return out
}
