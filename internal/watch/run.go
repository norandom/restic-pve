package watch

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/norandom/restic-pve/internal/stream/blockmap"
	"github.com/norandom/restic-pve/retro"
)

// Poll is how often the viewer reads the state file.
const Poll = 250 * time.Millisecond

// Idle is how long the viewer stays open without a running backup before it closes itself.
// A viewer whose terminal went away (a dropped ssh session) must not run for days.
const Idle = 10 * time.Minute

// ErrIdle is returned by Run when the viewer closed itself after Idle without a running backup.
var ErrIdle = errors.New("no running backup")

// Options configure a viewer.
type Options struct {
	Dir   string           // the guest view folder
	State string           // the state root: the local sources' status.json; "" leaves them out
	Words Words            // title and start command
	Color bool             // false: NO_COLOR
	Poll  time.Duration    // default Poll
	Idle  time.Duration    // default Idle
	Now   func() time.Time // default time.Now
	Ready func()           // tests: called after each drawn frame
}

// Run shows the view on an initialised screen until q, Q, Esc or Ctrl-C. It only reads the
// state directory.
func Run(screen tcell.Screen, o Options) error {
	if o.Poll <= 0 {
		o.Poll = Poll
	}
	if o.Idle <= 0 {
		o.Idle = Idle
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	t := retro.NewTheme(screen, o.Color)
	t.Apply()
	events := make(chan tcell.Event, 16)
	quit := make(chan struct{})
	defer close(quit)
	go screen.ChannelEvents(events, quit)
	tick := time.NewTicker(o.Poll)
	defer tick.Stop()

	var sp Speed
	var lsp LocalSpeed
	var watched time.Time // start of the run this viewer saw running
	lastActive := o.Now() // last time a backup was seen running
	var lastKey string    // what the last drawn frame showed
	// draw reads the state and redraws only when something visible changed: an idle viewer
	// must not send the same screen four times a second. force redraws regardless (resize).
	draw := func(force bool) {
		snap := Load(o.Dir)
		lc := LoadLocal(o.State)
		lsp.Add(lc.S)
		now := o.Now()
		key := "none"
		if lc.S != nil {
			key = fmt.Sprint("local", lc.Running, lc.S.RunStatus, lc.S.Updated, lc.S.CurrentUnit)
			if lc.Running {
				lastActive = now
			}
		}
		guestRunning := false
		if s := snap.Live; s != nil {
			sp.Add(s)
			if s.Status == blockmap.Running && now.Sub(s.Updated) <= Stale {
				watched = s.Started
				lastActive = now
				guestRunning = true
			}
			key = fmt.Sprint(key, s.Status, s.Started.UnixNano(), s.Updated.UnixNano())
		}
		cols, rows := screen.Size()
		// notices that depend on the clock (stopped updating, waiting) refresh every 5 s
		key = fmt.Sprint(key, cols, rows, now.Unix()/5)
		if !force && key == lastKey {
			if o.Ready != nil {
				o.Ready()
			}
			return
		}
		lastKey = key
		seen := snap.Live != nil && !watched.IsZero() && snap.Live.Started.Equal(watched)
		f := Build(snap, &sp, seen, now, cols, rows, o.Words)
		switch {
		case !guestRunning && lc.Running:
			f = BuildLocal(lc, &lsp, now, o.Words)
		case f.Notice != nil && lc.S != nil && !guestRunning && f.Notice.Heading == "No backup running":
			// what the local sources did last, before the start hint
			n := *f.Notice
			n.Lines = append(append(append([]string{}, n.Lines[:len(n.Lines)-1]...), upperFirst(localEnd(lc))+"."), n.Lines[len(n.Lines)-1])
			f.Notice = &n
		}
		if cols < 60 || rows < 20 {
			small(screen, t, LineAll(snap, lc, &lsp, now, o.Words.Start))
		} else {
			f.Draw(screen, t)
		}
		screen.Show()
		if o.Ready != nil {
			o.Ready()
		}
	}
	draw(true)
	for {
		select {
		case ev, ok := <-events:
			if !ok { // the terminal is gone
				return ErrIdle
			}
			switch ev := ev.(type) {
			case *tcell.EventKey:
				if ev.Key() == tcell.KeyCtrlC || ev.Key() == tcell.KeyEscape ||
					ev.Key() == tcell.KeyRune && (ev.Rune() == 'q' || ev.Rune() == 'Q') {
					return nil
				}
			case *tcell.EventResize:
				screen.Sync()
				draw(true)
			}
		case <-tick.C:
			draw(false)
			if o.Now().Sub(lastActive) > o.Idle {
				return ErrIdle
			}
		}
	}
}

// small is the screen of a terminal below 60 x 20: the plain line, wrapped.
func small(screen tcell.Screen, t retro.Theme, line string) {
	cols, rows := screen.Size()
	t.Background(screen)
	for i, l := range retro.Wrap(t.Text(line+" (q quits; the backup goes on)"), max(cols, 1)) {
		if i >= rows {
			break
		}
		retro.Put(screen, 0, i, cols, l, t.Base)
	}
	screen.HideCursor()
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
