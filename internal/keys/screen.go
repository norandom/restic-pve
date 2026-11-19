package keys

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/norandom/restic-pve/internal/tui"
	"github.com/norandom/restic-pve/retro"
)

// The notes under the table.
const (
	SigningNote = "Release signing key: belongs to the maintainer who publishes the tool, not on this machine. " +
		"You need it only to publish releases yourself."
	WhereNote = "The keys of this machine live in .env and secrets/."
)

// proof states of the screen
const (
	table = iota
	entering
	checking
	result
)

// Screen is the keys screen.
type Screen struct {
	Title  string
	Model  Model
	Back   string // the key bar's word for q: "Back" or "Quit"
	mode   int
	buf    []byte // the pasted copy while it is entered; wiped after use
	result Result
}

// Plain is the key table as plain lines: no value, presence and state only.
func (m Model) Plain() []string {
	var out []string
	for _, k := range m.Keys {
		cp := "no"
		if k.Copy {
			cp = "yes"
		}
		out = append(out, strings.Join([]string{"key", k.Name, k.State, "copy-away: " + cp, "kept-in: " + k.KeptIn,
			"if-lost: " + k.IfLost}, "  "))
	}
	out = append(out, "password-proof  "+m.ProofWords(time.Time{}))
	for _, o := range m.Others {
		out = append(out, "other  "+o.Name+"  "+o.State)
	}
	return append(out, "note  "+SigningNote, "note  "+WhereNote)
}

// ProofWords says the state of the copy; now zero gives a date without "today".
func (m Model) ProofWords(now time.Time) string {
	switch {
	case !m.Password:
		return "not possible: RESTIC_PASSWORD is not set"
	case m.Proof != nil:
		return "proven " + m.Proof.Local().Format("Mon 2 Jan 2006")
	case m.ProofStale:
		return "not proven for the password in use (the last proof was for another password)"
	}
	return "not proven yet"
}

// Draw draws the key table, and the proof box over it while a proof runs.
func (sc *Screen) Draw(s tcell.Screen, t retro.Theme) {
	cols, rows := s.Size()
	t.Background(s)
	if tui.Small(cols, rows) {
		for i, l := range tui.Rows(sc.Model.Plain(), cols) {
			if i < rows {
				retro.Put(s, 0, i, cols, l, t.Base)
			}
		}
		s.HideCursor()
		return
	}
	t.TitleBar(s, 0, cols, sc.Title, "")
	y := 1
	box := func(title string, h int) (x, w, top int) {
		b := t.Box(title)
		b.SetRect(1, y, cols-2, h+2)
		b.Draw(s)
		x, top, w, _ = b.GetInnerRect()
		y += h + 2
		return x + 1, w - 2, top
	}
	x, w, top := box("Your keys: only the first needs a copy away from this machine", 2*len(sc.Model.Keys))
	for i, k := range sc.Model.Keys {
		row := top + 2*i
		cp := "copy away: no"
		if k.Copy {
			cp = "copy away: YES"
		}
		retro.Put(s, x, row, 20, k.Name, t.Strong)
		retro.Put(s, x+21, row, 14, cp, map[bool]tcell.Style{true: t.Title, false: t.Base}[k.Copy])
		st := t.Base
		if k.Bad {
			st = t.Error
		}
		n := retro.Put(s, x+36, row, w-36, t.Text(k.State), st)
		retro.Put(s, x+36+n, row, w-36-n, ellipsis(t.Text(" · "+shortPath(k.KeptIn, w-36-n-3)), w-36-n), t.Base)
		retro.Put(s, x+1, row+1, w-1, ellipsis(t.Text(upper(k.Does)+". "+k.IfLost+"."), w-1), t.Base)
	}
	x, w, top = box("Your password copy", 2)
	pst := t.Base
	if sc.Model.Proof == nil {
		pst = t.Title
	}
	retro.Put(s, x, top, w, t.Text("Copy in your password manager: "+sc.Model.ProofWords(time.Now())+"."), pst)
	retro.Put(s, x, top+1, w, t.Text("Press p to prove it: paste the copy; nothing you paste is shown or stored."), t.Base)
	h := max(rows-1-y-2, 1)
	x, w, top = box("Also on this machine", h)
	var notes []string
	notes = append(notes, retro.Wrap(t.Text(SigningNote), w)...)
	var other []string
	for _, o := range sc.Model.Others {
		other = append(other, o.Name+" "+o.State)
	}
	notes = append(notes, retro.Wrap(t.Text("Other secrets here, in .env and secrets/: "+strings.Join(other, " · ")+"."), w)...)
	for i, l := range notes {
		if i < h {
			retro.Put(s, x, top+i, w, l, t.Base)
		}
	}
	t.BottomBar(s, rows-1, cols, " p Prove your password copy  q "+sc.Back, t.Bar, "restic-pve")
	if sc.mode != table {
		sc.drawProof(s, t)
	}
	s.HideCursor()
}

// shortPath keeps the last two parts of a long path ("…/rclone/rclone.conf [drive]") so the
// file name and the section stay readable; the plain lines carry the whole path.
func shortPath(s string, w int) string {
	if retro.Width(s) <= w || !strings.HasPrefix(s, "/") {
		return s
	}
	path, rest, _ := strings.Cut(s, " ")
	parts := strings.Split(path, "/")
	if len(parts) <= 3 {
		return s
	}
	short := "…/" + strings.Join(parts[len(parts)-2:], "/")
	if rest != "" {
		short += " " + rest
	}
	return short
}

func upper(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func ellipsis(s string, w int) string {
	if retro.Width(s) <= w {
		return s
	}
	return retro.Cut(s, w-1) + "…"
}

func (sc *Screen) drawProof(s tcell.Screen, t retro.Theme) {
	cols, rows := s.Size()
	w := min(cols-4, 72)
	lines := retro.Wrap(t.Text(ProofIntro), w-4)
	h := len(lines) + 8
	x, y := (cols-w)/2, max((rows-h)/2, 1)
	retro.Fill(s, x, y, w, h, ' ', t.Base)
	b := t.Box("Prove your password copy")
	b.SetRect(x, y, w, h)
	b.Draw(s)
	for i, l := range lines {
		retro.Put(s, x+2, y+1+i, w-4, l, t.Base)
	}
	fy := y + 2 + len(lines)
	retro.Put(s, x+2, fy, 2, ">", t.Base)
	retro.Fill(s, x+4, fy, w-6, 1, ' ', t.Field)
	if len(sc.buf) > 0 || sc.mode != entering {
		retro.Put(s, x+4, fy, w-6, Mask, t.Field) // fixed: the length is never shown
	} else {
		retro.Put(s, x+4, fy, w-6, "paste the repository password here", t.Example)
	}
	msg, st := "Enter checks it · Esc cancels", t.Strong
	switch sc.mode {
	case checking:
		msg = "Checking the copy against every repository…"
	case result:
		msg = sc.result.Text
		if !sc.result.OK {
			st = t.Error
		}
	}
	for i, l := range retro.Wrap(t.Text(msg), w-4) {
		if fy+2+i < y+h-1 {
			retro.Put(s, x+2, fy+2+i, w-4, l, st)
		}
	}
	if sc.mode == result {
		retro.Put(s, x+2, y+h-2, w-4, "Any key closes this box.", t.Base)
	}
}

// wipe zeroes the pasted bytes.
func (sc *Screen) wipe() {
	clear(sc.buf)
	sc.buf = nil
}

// Options are what the keys screen needs from the command layer.
type Options struct {
	Screen tcell.Screen
	Color  bool
	Title  string
	Back   string
	Model  func() Model // reads the keys again (after a proof)
	Prover Prover
	Poll   time.Duration
	Idle   time.Duration
	Now    func() time.Time
	Ready  func()
}

// Run shows the keys screen until q, Esc or Ctrl-C (tui.ErrIdle after ten idle minutes).
// Whatever ends it, the pasted bytes are wiped.
func Run(o Options) error {
	t := retro.NewTheme(o.Screen, o.Color)
	sc := &Screen{Title: o.Title, Model: o.Model(), Back: o.Back}
	defer sc.wipe()
	o.Screen.EnablePaste()
	defer o.Screen.DisablePaste()
	return tui.Loop{
		Screen: o.Screen, Theme: t, Poll: o.Poll, Idle: o.Idle, Now: o.Now, Ready: o.Ready,
		Draw: func(s tcell.Screen) { sc.Draw(s, t) },
		Leave: func(ev *tcell.EventKey) bool {
			return sc.mode == table && (ev.Key() == tcell.KeyEscape || ev.Key() == tcell.KeyRune && (ev.Rune() == 'q' || ev.Rune() == 'Q'))
		},
		Key:   func(ev *tcell.EventKey) bool { return sc.key(ev, o) },
		Paste: func(bool) {}, // a paste counts as a key for the idle time
	}.Run()
}

func (sc *Screen) key(ev *tcell.EventKey, o Options) bool {
	switch sc.mode {
	case table:
		if ev.Key() == tcell.KeyRune && (ev.Rune() == 'p' || ev.Rune() == 'P') {
			sc.mode = entering
			return true
		}
		return false
	case checking:
		return false
	case result:
		sc.mode = table
		sc.Model = o.Model()
		return true
	}
	switch ev.Key() {
	case tcell.KeyRune:
		sc.buf = utf8.AppendRune(sc.buf, ev.Rune())
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if len(sc.buf) > 0 {
			_, n := utf8.DecodeLastRune(sc.buf)
			clear(sc.buf[len(sc.buf)-n:])
			sc.buf = sc.buf[:len(sc.buf)-n]
		}
	case tcell.KeyCtrlU:
		clear(sc.buf)
		sc.buf = sc.buf[:0]
	case tcell.KeyEscape:
		sc.wipe()
		sc.mode = table
	case tcell.KeyEnter:
		if len(sc.buf) == 0 {
			return false
		}
		copy := sc.buf
		sc.buf = nil
		sc.mode = checking
		go func() {
			r := o.Prover.Prove(copy) // wipes copy
			tui.Post(o.Screen, func() { sc.result, sc.mode = r, result })
		}()
	default:
		return false
	}
	return true
}
