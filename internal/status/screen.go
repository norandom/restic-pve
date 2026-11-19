package status

import (
	"fmt"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/norandom/restic-pve/internal/tui"
	"github.com/norandom/restic-pve/retro"
)

// Screen is the status screen. Model is replaced when the facts are read again; Busy names
// the read that runs now ("" when none).
type Screen struct {
	Title   string // "restic-pve <version> · backup machine · Status"
	Model   Model
	Busy    string
	Details bool // Enter: the full list of copies
	Top     int  // first row of the full list
	Help    bool // ?: the help box
	Zone    string
}

// fixed is the height of everything but the rows of the first box and the verdict line.
const fixed = 1 + 2 + 5 + 7 + 1

func (sc *Screen) checked() string {
	f := sc.Model.F
	switch {
	case sc.Busy != "":
		return sc.Busy
	case f.CheckedAt.IsZero():
		return "storage not read"
	}
	ago := sc.Model.Now.Sub(f.CheckedAt)
	var s string
	switch {
	case ago < time.Minute:
		s = "checked just now"
	case ago < time.Hour:
		s = fmt.Sprintf("checked %d min ago", int(ago.Minutes()))
	default:
		s = fmt.Sprintf("checked %d h ago", int(ago.Hours()))
	}
	if f.Deferred {
		s += ", next read after the backup"
	}
	return s
}

// local formats a time in the machine's zone.
func local(t time.Time, layout string) string { return t.Local().Format(layout) }

func (sc *Screen) copyLine(r Row) (string, string) {
	word := map[Verdict]string{Safe: "yes", NotSafe: "NO", Unknown: "?"}[r.Verdict]
	kind := r.In.Kind
	age := days(r.Age)
	detail := ""
	switch r.Verdict {
	case Safe:
		detail = local(*r.At, "Mon 2 Jan 15:04")
		if r.Note != "" {
			detail += " · " + r.Note
		}
	case NotSafe:
		detail = r.Reason + ": " + r.Do
	default:
		age, detail = "-", r.Reason+": "+r.Do
	}
	return word, fmt.Sprintf("%-6s %-16s %-9s %s", kind, r.In.Name, age, detail)
}

// Draw draws the screen; a terminal below 60 x 20 gets the plain lines.
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
	f := m.F
	t.TitleBar(s, 0, cols, sc.Title, sc.checked())
	extra := rows - fixed
	verdictH := 1
	if extra >= 6 {
		verdictH = 2
	}
	// the copies box takes the rows that are left, up to one per copy
	n := min(max(len(sc.Model.Rows), 1), extra-verdictH)
	y := 1
	box := func(title string, h int) (x, w, top int) {
		b := t.Box(title)
		b.SetRect(1, y, cols-2, h+2)
		b.Draw(s)
		x, top, w, _ = b.GetInnerRect()
		y += h + 2
		return x + 1, w - 2, top
	}
	// 1: copies
	x, w, top := box("Is there a recent copy away from the site?", n)
	shown := m.Rows
	more := 0
	if len(shown) > n {
		shown, more = shown[:n-1], len(shown)-(n-1)
	}
	for i, r := range shown {
		word, rest := sc.copyLine(r)
		st := t.Strong
		if r.Verdict != Safe {
			st = t.Error
		}
		retro.Put(s, x, top+i, 2, t.Text("■"), st)
		retro.Put(s, x+2, top+i, 4, word, st)
		retro.Put(s, x+7, top+i, w-7, ellipsis(t.Text(rest), w-7), t.Base)
	}
	if more > 0 {
		retro.Put(s, x+2, top+len(shown), w-2, t.Text(fmt.Sprintf("… %d more: press Enter", more)), t.Title)
	}
	if len(m.Rows) == 0 {
		retro.Put(s, x, top, w, "No source and no guest in backup.toml yet.", t.Base)
	}
	// 2: restore
	x, w, top = box("Have I seen a restore work?", 3)
	test := "none recorded yet; the monthly test records its result here"
	if rt := f.RestoreTest; rt != nil {
		test = fmt.Sprintf("%s  %s", local(rt.TS, "Mon 2 Jan"), restoreWords[rt.Status])
		if rt.Unit != "" {
			test += " (" + unitName(rt.Unit) + ")"
		}
	}
	drill := "none recorded. Do one: " + DrillGuide
	if f.Drill != nil {
		drill = local(*f.Drill, "Mon 2 Jan 2006")
		if m.Now.Sub(*f.Drill) > 183*24*time.Hour {
			drill += ", over six months ago. Do one: " + DrillGuide
		}
	}
	for i, l := range []string{"Automatic restore test  " + test, "Drill (you checked it)  " + drill,
		"On the bad day          " + BadDayGuide} {
		retro.Put(s, x, top+i, w, t.Text(l), t.Base)
	}
	// 3: now and next
	x, w, top = box("Now and next", 5)
	retro.Put(s, x, top, w, t.Text("Running   "+sc.running()), t.Base)
	if len(f.Running) > 0 {
		retro.LeftRight(s, x, top, w, "", "w to watch", t.Title)
	}
	retro.Put(s, x, top+1, w, t.Text("Next      "+sc.next()), t.Base)
	spaceSt := t.Base
	if strings.Contains(SpaceLine(f.Space), "LOW") {
		spaceSt = t.Error
	}
	retro.Put(s, x, top+2, w, t.Text("Space     "+SpaceLine(f.Space)), spaceSt)
	retro.Put(s, x, top+3, w, t.Text(fmt.Sprintf("Updates   %s · updated with this machine's image", f.Version)), t.Base)
	retro.LeftRight(s, x, top+3, w, "", "Alerts  "+onOff(f.Alerts), t.Base)
	pw := "copy not proven: press k (Keys) and prove it"
	if f.Proof != nil {
		pw = "copy proven " + local(*f.Proof, "Mon 2 Jan 2006")
	}
	retro.Put(s, x, top+4, w, t.Text("Password  "+pw), t.Base)
	// verdict and key bar
	vs := t.Strong
	if m.Verdict != Safe {
		vs = t.Title
	}
	vl := retro.Wrap(t.Text(m.VerdictLine()), cols-2)
	vl = vl[:min(len(vl), verdictH)]
	for i, l := range vl { // right under the boxes
		retro.Put(s, 1, y+i, cols-2, l, vs)
	}
	keys := " Enter details  k Keys  s Space  w Watch  r Refresh  ? Help  q Quit"
	t.BottomBar(s, rows-1, cols, keys, t.Bar, "restic-pve")
	switch {
	case sc.Help:
		tui.Overlay(s, t, "Help", sc.helpLines(), "Any key closes this box.", true)
	case sc.Details:
		sc.drawDetails(s, t)
	}
	s.HideCursor()
}

func (sc *Screen) running() string {
	f := sc.Model.F
	var parts []string
	for _, j := range f.Running {
		if j.Started.IsZero() {
			parts = append(parts, j.Name+" runs now")
		} else {
			parts = append(parts, j.Name+" since "+local(j.Started, "Mon 15:04"))
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, " · ")
	}
	if len(f.LastJobs) == 0 {
		return "nothing now"
	}
	j := f.LastJobs[0]
	if j.Ended == nil {
		return fmt.Sprintf("nothing now; last: %s ended without a result (stopped or out of memory)", j.Name)
	}
	return fmt.Sprintf("nothing now; last: %s ended %s, %s", j.Name, local(*j.Ended, "Mon 2 Jan 15:04"), exitWord(j.Exit))
}

func (sc *Screen) next() string {
	var parts []string
	for _, n := range sc.Model.F.Next {
		name := map[string]string{"local": "local sources", "guests": "guests"}[n.Profile]
		if n.At.IsZero() {
			parts = append(parts, name+" not known")
		} else {
			parts = append(parts, name+" "+local(n.At, "Mon 2 Jan 15:04"))
		}
	}
	if len(parts) == 0 {
		return "nothing scheduled"
	}
	return strings.Join(parts, " · ")
}

func (sc *Screen) helpLines() []string {
	l := []string{
		"Each row is one source or guest. \"yes\" means its newest copy that passed an integrity check is " +
			"younger than its schedule interval plus two days.",
		"Times are in this machine's time zone (" + sc.Zone + "). \"Next ... not known\": backup.toml has no [schedule] line for it.",
		"",
		"On the bad day (the machine or the site is lost): " + BadDayGuide + ".",
		"Check that a restore works yourself once every six months: " + DrillGuide + ".",
		"",
		"Keys: k shows every key and proves your password copy. Space: s. Watch the backup that runs: w. " +
			"r reads the storage now; otherwise it is read at most every six hours.",
	}
	for _, e := range sc.Model.F.Errors {
		l = append(l, "", "Problem: "+e.Text)
	}
	return l
}

func (sc *Screen) drawDetails(s tcell.Screen, t retro.Theme) {
	cols, rows := s.Size()
	var lines []string
	for _, r := range sc.Model.Rows {
		word, rest := sc.copyLine(r)
		lines = append(lines, fmt.Sprintf("%-4s %s", word, rest))
	}
	h := rows - 4
	sc.Top = max(min(sc.Top, len(lines)-(h-4)), 0)
	end := min(sc.Top+h-4, len(lines))
	tui.Overlay(s, t, fmt.Sprintf("All copies (%d)", len(lines)), lines[sc.Top:end],
		"Up/Down scroll · Enter or Esc closes this list", false)
	_ = cols
}

// Key handles a key of the screen: Enter, Up/Down in the list, ?. It returns true when the
// screen changed; the command layer handles k, s, w, r and q.
func (sc *Screen) Key(ev *tcell.EventKey) bool {
	switch {
	case sc.Help:
		sc.Help = false
		return true
	case sc.Details:
		switch ev.Key() {
		case tcell.KeyUp:
			sc.Top--
		case tcell.KeyDown:
			sc.Top++
		case tcell.KeyEnter:
			sc.Details = false
		default:
			return false
		}
		return true
	case ev.Key() == tcell.KeyEnter:
		sc.Details, sc.Top = true, 0
		return true
	case ev.Key() == tcell.KeyRune && ev.Rune() == '?':
		sc.Help = true
		return true
	}
	return false
}

// Options are what the status screen needs from the command layer.
type Options struct {
	Screen tcell.Screen
	Color  bool
	Title  string
	Zone   string
	// Read reads the facts: refresh reads the storage now; cacheOnly never reads it.
	Read  func(refresh, cacheOnly bool) Model
	Stamp func() string // the local files the facts come from (tui.FileStamp)
	Now   func() time.Time
	Poll  time.Duration
	Idle  time.Duration
	Ready func()
}

// Run shows the status screen. first is the model from the local files and the cache; the
// storage is read in the background when the cache asks for it. It returns the screen to
// open next ("keys", "space", "watch", or "" to quit), and tui.ErrIdle when it closed itself.
func Run(o Options, first Model) (string, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	t := retro.NewTheme(o.Screen, o.Color)
	sc := &Screen{Title: o.Title, Model: first, Zone: o.Zone, Busy: "reading…"}
	reading := false
	read := func(refresh bool) {
		reading = true
		go func() {
			m := o.Read(refresh, false)
			tui.Post(o.Screen, func() { sc.Model, sc.Busy, reading = m, "", false })
		}()
	}
	read(false)
	next := ""
	err := tui.Loop{
		Screen: o.Screen, Theme: t, Poll: o.Poll, Idle: o.Idle, Now: o.Now, Ready: o.Ready,
		Draw: func(s tcell.Screen) {
			sc.Model.Now = o.Now()
			sc.Draw(s, t)
		},
		Leave: func(ev *tcell.EventKey) bool {
			if sc.Help || sc.Details {
				return false
			}
			if ev.Key() == tcell.KeyRune {
				switch ev.Rune() {
				case 'k', 'K':
					next = "keys"
				case 's', 'S':
					next = "space"
				case 'w', 'W':
					next = "watch"
				case 'q', 'Q':
					return true
				}
				return next != ""
			}
			return ev.Key() == tcell.KeyEscape
		},
		Key: func(ev *tcell.EventKey) bool {
			if sc.Details && ev.Key() == tcell.KeyEscape {
				sc.Details = false
				return true
			}
			if !sc.Help && !sc.Details && ev.Key() == tcell.KeyRune && (ev.Rune() == 'r' || ev.Rune() == 'R') {
				if !reading {
					sc.Busy = "reading the storage…"
					read(true)
				}
				return true
			}
			return sc.Key(ev)
		},
		Stamp: func() string {
			s := ""
			if o.Stamp != nil {
				s = o.Stamp()
			}
			return s + o.Now().Format("15:04") // "N min ago" changes once a minute
		},
		Changed: func() {
			if !reading {
				sc.Model = o.Read(false, true)
			}
		},
	}.Run()
	return next, err
}

// ellipsis cuts s to w columns and marks the cut.
func ellipsis(s string, w int) string {
	if retro.Width(s) <= w {
		return s
	}
	return retro.Cut(s, w-1) + "…"
}
