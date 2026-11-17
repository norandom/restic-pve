package install

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/gdamore/tcell/v2"
	"github.com/norandom/restic-pve/retro"
	"github.com/rivo/tview"
)

// The examples shown in the form (grey in an empty field, and in the Help box) and in the
// plain questions. An example is never taken as a value.
const (
	exampleFrom = "192.0.2.10"
	exampleKey  = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI..."
)

// The smallest terminal the form is drawn on; a smaller one gets the plain questions.
const minCols, minRows = 60, 20

// Terminal is the terminal the form runs on: terminal.go on a host, a fake in tests.
type Terminal interface {
	// Size returns the columns and rows.
	Size() (cols, rows int, err error)
	// Screen returns a new screen on the terminal, not yet initialised.
	Screen() (tcell.Screen, error)
	// Watch restores the terminal with fini when a signal ends the program; the returned
	// stop ends the watch.
	Watch(fini func()) (stop func())
}

// errNoForm means the terminal could not be set up; install asks line by line instead.
var errNoForm = errors.New("no form")

// useForm reports whether install shows the form: not --plain, stdin and stdout are a
// terminal, TERM is set and not dumb, and the terminal is at least minCols x minRows.
func (s *System) useForm() bool {
	if s.Plain || s.Term == nil || s.Getenv == nil {
		return false
	}
	if t := s.Getenv("TERM"); t == "" || t == "dumb" {
		return false
	}
	cols, rows := s.termSize()
	return cols >= minCols && rows >= minRows
}

// termSize is the terminal size, 80x24 when it cannot be read.
func (s *System) termSize() (int, int) {
	cols, rows, err := s.Term.Size()
	if err != nil || cols <= 0 || rows <= 0 {
		return 80, 24
	}
	return cols, rows
}

// form shows the install form and returns the values to install. The terminal is restored
// before it returns, also on a panic or a signal.
func (s *System) form(def *Values, gs []guest, sts []storage) (*Values, error) {
	screen, err := s.Term.Screen()
	if err != nil {
		return nil, errNoForm
	}
	if err := screen.Init(); err != nil {
		return nil, errNoForm
	}
	defer screen.Fini()
	defer s.Term.Watch(screen.Fini)()
	return newForm(def, gs, sts).run(screen, s.Getenv("NO_COLOR") == "")
}

// keyKind is one key of the form.
type keyKind int

const (
	kRune keyKind = iota
	kEnter
	kEsc
	kBackTab
	kTab
	kUp
	kDown
	kLeft
	kRight
	kBackspace
	kCtrlC
	kCtrlU
	kNone
)

type keyIn struct {
	kind keyKind
	r    rune
}

// keyOf turns a key event of the terminal into a key of the form.
func keyOf(ev *tcell.EventKey) keyIn {
	switch ev.Key() {
	case tcell.KeyRune:
		if unicode.IsPrint(ev.Rune()) {
			return keyIn{kind: kRune, r: ev.Rune()}
		}
	case tcell.KeyEnter, tcell.KeyLF:
		return keyIn{kind: kEnter}
	case tcell.KeyEsc:
		return keyIn{kind: kEsc}
	case tcell.KeyBacktab:
		return keyIn{kind: kBackTab}
	case tcell.KeyTab:
		return keyIn{kind: kTab}
	case tcell.KeyUp:
		return keyIn{kind: kUp}
	case tcell.KeyDown:
		return keyIn{kind: kDown}
	case tcell.KeyLeft:
		return keyIn{kind: kLeft}
	case tcell.KeyRight:
		return keyIn{kind: kRight}
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		return keyIn{kind: kBackspace}
	case tcell.KeyCtrlC:
		return keyIn{kind: kCtrlC}
	case tcell.KeyCtrlU:
		return keyIn{kind: kCtrlU}
	}
	return keyIn{kind: kNone}
}

// pasteKeys are the keys of a paste: its first line, and Enter when a line break ends it.
// The rest after the first line break is dropped.
func pasteKeys(text string) []keyIn {
	var ks []keyIn
	for _, r := range text {
		if r == '\r' || r == '\n' {
			return append(ks, keyIn{kind: kEnter})
		}
		if unicode.IsPrint(r) {
			ks = append(ks, keyIn{kind: kRune, r: r})
		}
	}
	return ks
}

// The steps of the form; the summary is not counted in "step n of 5".
const (
	stepFrom = iota
	stepKey
	stepGuests
	stepStorage
	stepLimits
	stepSummary
)

var stepTitles = []string{"Backup machine address", "Public key", "Guests", "Fleecing storage", "Limits"}

var stepTexts = []string{
	"Type the IP address of the backup machine: the machine that runs the backups and keeps them. " +
		"Only this address may log in with the backup key. " +
		"Run ./setup.sh --proxmox on the backup machine; it prints this address.",
	"Paste the whole public key line of the backup machine. It starts with ssh-ed25519 and is " +
		"printed by ./setup.sh --proxmox on the backup machine. A public key is not a secret.",
	"Tick the virtual machines to back up. The backup machine can read only the ticked ones.",
	"Choose where this host keeps temporary data while a running guest is read, so the guest " +
		"is not slowed down (fleecing). The usual choice is the one with the most free space on fast disks.",
	"These limits protect this host while a backup runs. The current values suit most hosts.",
}

const summaryText = "Install writes these values, a backup user with its key, a sudoers file, " +
	"the binary and a daily update timer. Nothing changes before you choose Install."

// The short help in the Help box, per step.
const (
	helpFrom    = "Example: " + exampleFrom
	helpKey     = "Example: ssh-ed25519 AAAA... and an optional comment, all on one line."
	helpLimits  = "Keep: the values above stay. Change: edit them one by one."
	helpSummary = "Install starts. Back returns to the limits. Cancel changes nothing."
)

// The four limit fields of "Change": label and one sentence with the allowed range.
var limitFields = []struct{ label, help string }{
	{"Free space to start (GiB)", "A backup starts only when the fleecing storage has this much free space. 1-999999."},
	{"Free space to stop (GiB)", "A running backup stops when free space falls below this. Lower than the start value, 1-999999."},
	{"Read limit (MiB/s)", "How fast a backup may read the disks of a guest. 1-" + strconv.Itoa(maxBwlimitMiB) + "."},
	{"Backups at the same time", "How many guests are backed up at once. 1-16."},
}

var (
	limitChoices = []string{"Keep these (recommended)", "Change"}
	buttons      = []string{retro.Button("Install"), retro.Button("Back"), retro.Button("Cancel")}
)

// The keys on the bottom bar; Ctrl-C cancel is on the title bar.
const (
	footerText    = "Enter continue · Esc back · Ctrl-U clear"
	footerTicks   = "Up/Down move · Space tick · Enter continue · Esc back"
	footerChoice  = "Up/Down move · Enter choose · Esc back"
	footerFields  = "Up/Down field · Enter next · Esc back · Ctrl-U clear"
	footerSummary = "Left/Right move · Enter press · Esc back"
	cancelKey     = "Ctrl-C=Cancel"
	programName   = "restic-pve"
)

type form struct {
	v      Values
	gs     []guest
	sts    []storage
	onHost map[int]bool
	active map[string]bool

	step      int
	from, key string
	ticked    []int // in the order they were ticked; a reinstall starts with VMIDS
	gCur      int
	gTop      int
	sCur      int
	sTop      int
	limCur    int // 0 keep, 1 change
	editing   bool
	lim       []string
	field     int
	button    int
	msg       string

	t      retro.Theme
	done   bool  // Install was chosen
	err    error // the form ended without values
	stopFn func()
}

func newForm(def *Values, gs []guest, sts []storage) *form {
	f := &form{v: *def, gs: gs, sts: sts, stopFn: func() {},
		onHost: map[int]bool{}, active: map[string]bool{}, from: def.From, key: def.Pubkey}
	for _, g := range gs {
		n, _ := strconv.Atoi(g.vmid)
		f.onHost[n] = true
	}
	for _, id := range def.VMIDs {
		if f.onHost[id] {
			f.ticked = append(f.ticked, id)
		}
	}
	for i, st := range sts {
		f.active[st.name] = true
		if st.name == def.Storage {
			f.sCur = i
		}
	}
	return f
}

// formView is the form as a tview primitive: it draws the whole screen and takes pastes.
type formView struct {
	*tview.Box
	f *form
}

func (v *formView) Draw(screen tcell.Screen) { v.f.draw(screen) }

func (v *formView) PasteHandler() func(string, func(tview.Primitive)) {
	return func(text string, _ func(tview.Primitive)) {
		for _, k := range pasteKeys(text) {
			if v.f.press(k) {
				return
			}
		}
	}
}

// run shows the form on an initialised screen until Install, Cancel, Ctrl-C or the end of
// the terminal.
func (f *form) run(screen tcell.Screen, color bool) (*Values, error) {
	f.t = retro.NewTheme(screen, color)
	f.t.Apply()
	app := tview.NewApplication().SetScreen(retro.Ready(screen)).EnablePaste(true)
	f.stopFn = app.Stop
	app.SetRoot(&formView{Box: tview.NewBox(), f: f}, true)
	app.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		f.press(keyOf(ev))
		return nil
	})
	_ = app.Run() // an error is the terminal ending
	switch {
	case f.done:
		v := f.v
		return &v, nil
	case f.err != nil:
		return nil, f.err
	}
	return nil, errStop("the terminal closed")
}

// press handles one key and reports whether the form ended.
func (f *form) press(k keyIn) bool {
	if f.done || f.err != nil {
		return true
	}
	if k.kind == kCtrlC {
		f.err = errStop("cancelled")
	} else if done, err := f.handle(k); err != nil {
		f.err = err
	} else {
		f.done = done
	}
	if f.done || f.err != nil {
		f.stopFn()
		return true
	}
	return false
}

func (f *form) handle(k keyIn) (bool, error) {
	if k.kind == kNone {
		return false, nil
	}
	f.msg = ""
	if k.kind == kEsc || k.kind == kBackTab {
		f.back()
		return false, nil
	}
	switch f.step {
	case stepFrom:
		if edit(&f.from, k) {
			x := strings.TrimSpace(f.from)
			f.ok(checkFrom(x), func() { f.v.From = x })
		}
	case stepKey:
		if edit(&f.key, k) {
			pub, err := checkPubkey(strings.TrimSpace(f.key))
			f.ok(err, func() { f.v.Pubkey = pub })
		}
	case stepGuests:
		f.guestKey(k)
	case stepStorage:
		switch k.kind {
		case kUp, kDown, kRune:
			f.sCur = move(f.sCur, len(f.sts), k)
		case kEnter:
			name := f.sts[f.sCur].name
			f.ok(checkStorage(name, f.active), func() { f.v.Storage = name })
		}
	case stepLimits:
		f.limitKey(k)
	case stepSummary:
		switch k.kind {
		case kLeft:
			f.button = (f.button + len(buttons) - 1) % len(buttons)
		case kRight, kTab:
			f.button = (f.button + 1) % len(buttons)
		case kEnter:
			switch f.button {
			case 0:
				return true, nil
			case 1:
				f.back()
			default:
				return false, errStop("cancelled")
			}
		}
	}
	return false, nil
}

// ok shows err as the rule, or sets the value and goes to the next step.
func (f *form) ok(err error, set func()) {
	if err != nil {
		f.msg = err.Error()
		return
	}
	set()
	f.step++
	f.button = 0
}

func (f *form) back() {
	switch {
	case f.step == stepLimits && f.editing:
		f.editing = false
	case f.step > stepFrom:
		f.step--
		f.editing = false
	}
}

// edit applies k to a text field and reports whether it was Enter.
func edit(s *string, k keyIn) bool {
	switch k.kind {
	case kRune:
		*s += string(k.r)
	case kBackspace:
		if r := []rune(*s); len(r) > 0 {
			*s = string(r[:len(r)-1])
		}
	case kCtrlU:
		*s = ""
	case kEnter:
		return true
	}
	return false
}

// move moves a list cursor with Up/Down or j/k.
func move(cur, n int, k keyIn) int {
	switch {
	case k.kind == kUp || k.kind == kRune && k.r == 'k':
		if cur > 0 {
			cur--
		}
	case k.kind == kDown || k.kind == kRune && k.r == 'j':
		if cur < n-1 {
			cur++
		}
	}
	return cur
}

func (f *form) guestKey(k keyIn) {
	switch {
	case k.kind == kRune && k.r == ' ':
		id, _ := strconv.Atoi(f.gs[f.gCur].vmid)
		for i, t := range f.ticked {
			if t == id {
				f.ticked = append(f.ticked[:i:i], f.ticked[i+1:]...)
				return
			}
		}
		f.ticked = append(f.ticked, id)
	case k.kind == kEnter:
		ids := make([]string, len(f.ticked))
		for i, id := range f.ticked {
			ids[i] = strconv.Itoa(id)
		}
		vmids, err := parseVMIDs(strings.Join(ids, " "), f.onHost)
		f.ok(err, func() { f.v.VMIDs = vmids })
	default:
		f.gCur = move(f.gCur, len(f.gs), k)
	}
}

func (f *form) limitKey(k keyIn) {
	if !f.editing {
		switch k.kind {
		case kUp, kDown, kRune:
			f.limCur = move(f.limCur, len(limitChoices), k)
		case kEnter:
			if f.limCur == 0 {
				f.step++
				f.button = 0
				return
			}
			f.editing, f.field = true, 0
			f.lim = []string{strconv.Itoa(f.v.MinFreeGiB), strconv.Itoa(f.v.AbortFreeGiB),
				strconv.Itoa(roundMiB(f.v.BwlimitKiB)), strconv.Itoa(f.v.MaxWorkers)}
		}
		return
	}
	switch k.kind {
	case kUp:
		f.field = max(f.field-1, 0)
	case kDown, kTab:
		f.field = min(f.field+1, len(f.lim)-1)
	default:
		if !edit(&f.lim[f.field], k) {
			return
		}
		at, v, err := f.checkLimits()
		switch {
		case err != nil && at <= f.field:
			f.field, f.msg = at, err.Error()
		case f.field < len(f.lim)-1:
			f.field++
		case err != nil:
			f.field, f.msg = at, err.Error()
		default:
			f.v.MinFreeGiB, f.v.AbortFreeGiB, f.v.BwlimitKiB, f.v.MaxWorkers = v[0], v[1], v[2]*1024, v[3]
			f.editing = false
			f.step++
			f.button = 0
		}
	}
}

// checkLimits checks the four limit fields in order; at is the first invalid one.
func (f *form) checkLimits() (at int, v [4]int, err error) {
	t := make([]string, len(f.lim))
	for i, s := range f.lim {
		t[i] = strings.TrimSpace(s)
	}
	if v[0], err = checkNumber("FLEECING_MIN_FREE_GIB", t[0]); err != nil {
		return 0, v, err
	}
	if v[1], err = checkAbort(t[1], v[0]); err != nil {
		return 1, v, err
	}
	if v[2], err = checkBwlimitMiB(t[2]); err != nil {
		return 2, v, err
	}
	if v[3], err = checkNumber("MAX_WORKERS", t[3]); err != nil {
		return 3, v, err
	}
	return len(t), v, nil
}

// ---- drawing ----

// seg is a piece of a line in one style.
type seg struct {
	s  string
	st tcell.Style
}

// body collects the lines of the main box and the place of the text cursor.
type body struct {
	t          retro.Theme
	w          int // columns of a line
	lines      [][]seg
	fill       []bool // the line's last style runs to the end of the line
	curRow     int    // line of the cursor; -1 hides it
	curCol     int
	bodyHeight int // lines left for the list or fields
}

func (b *body) add(fill bool, segs ...seg) {
	b.lines = append(b.lines, segs)
	b.fill = append(b.fill, fill)
}

func (b *body) text(s string) { b.add(false, seg{s, b.t.Base}) }

func (b *body) para(text string) {
	for _, l := range retro.Wrap(text, b.w) {
		b.text(l)
	}
}

// mark is the pointer in front of the focused row.
func (f *form) mark(on bool) string {
	if on {
		return string(f.t.G.Current) + " "
	}
	return "  "
}

// row adds a list row: the focused one with the pointer and the yellow highlight.
func (f *form) row(b *body, focused bool, text string) {
	if focused {
		b.add(true, seg{f.mark(true) + text, f.t.Focus})
	} else {
		b.text(f.mark(false) + text)
	}
}

func (f *form) draw(screen tcell.Screen) {
	cols, rows := screen.Size()
	t := f.t
	l := retro.NewLayout(cols, rows)
	t.Background(screen)

	title := "restic-pve install · "
	boxTitle, help := "Summary", helpSummary
	if f.step < stepSummary {
		title += fmt.Sprintf("step %d of %d: %s", f.step+1, len(stepTitles), stepTitles[f.step])
		boxTitle = stepTitles[f.step]
	} else {
		title += "summary"
	}
	t.TitleBar(screen, l.TitleBarY, cols, title, cancelKey)

	// the main box: the explanation, then the field, the list or the summary
	main := t.Box(boxTitle)
	main.SetRect(1, l.BodyY, cols-2, l.BodyH)
	main.Draw(screen)
	bx, by, inner := 3, l.BodyY+1, l.BodyH-2
	b := &body{t: t, w: cols - 6, curRow: -1}
	if f.step < stepSummary {
		b.para(stepTexts[f.step])
		b.text("")
	} else {
		b.para(summaryText)
	}
	b.bodyHeight = inner - len(b.lines)
	footer := footerChoice
	switch f.step {
	case stepFrom:
		footer, help = footerText, helpFrom
		f.drawField(b, f.from, exampleFrom)
	case stepKey:
		footer, help = footerText, helpKey
		f.drawField(b, f.key, exampleKey)
	case stepGuests:
		footer, help = footerTicks, fmt.Sprintf("%d ticked", len(f.ticked))
		f.drawGuests(b)
	case stepStorage:
		st := f.sts[f.sCur]
		help = fmt.Sprintf("%s: %s, %d GiB free", st.name, st.typ, st.freeGiB)
		f.drawStorages(b)
	case stepLimits:
		help = helpLimits
		if f.editing {
			footer, help = footerFields, limitFields[f.field].help
		}
		f.drawLimits(b)
	case stepSummary:
		footer = footerSummary
		f.drawSummary(b)
	}
	for i, segs := range b.lines {
		if i >= inner {
			break
		}
		x := bx
		for _, s := range segs {
			x += retro.Put(screen, x, by+i, bx+b.w-x, s.s, s.st)
		}
		if b.fill[i] && len(segs) > 0 {
			retro.Fill(screen, x, by+i, bx+b.w-x, 1, ' ', segs[len(segs)-1].st)
		}
	}

	// the lower boxes: where the admin is, and the help or the rule that was broken
	f.drawProgress(screen, l)
	hb := t.Box("Help")
	hb.SetRect(l.RightX, l.LowerY, l.RightW(), retro.LowerHeight)
	hb.Draw(screen)
	hx, hy, hw, hh := hb.GetInnerRect()
	hx, hw = hx+1, hw-2
	var hl []seg
	if f.msg != "" {
		for _, s := range retro.Wrap(f.msg, hw) {
			hl = append(hl, seg{s, t.Error})
		}
	}
	// the help shows below the rule only when all of it fits
	if hlp := retro.Wrap(help, hw); len(hl) == 0 || len(hl)+len(hlp) <= hh {
		for _, s := range hlp {
			hl = append(hl, seg{s, t.Base})
		}
	}
	for i := 0; i < len(hl) && i < hh; i++ {
		retro.Put(screen, hx, hy+i, hw, hl[i].s, hl[i].st)
	}

	t.BottomBar(screen, l.BottomBarY, cols, footer, t.Bar, programName)

	if b.curRow >= 0 && b.curRow < inner && b.curRow < len(b.lines) {
		screen.ShowCursor(bx+b.curCol, by+b.curRow)
	} else {
		screen.HideCursor()
	}
}

// drawProgress draws the left lower box: the step and its share of the way, a track of
// the steps done, the step's name and the job.
func (f *form) drawProgress(screen tcell.Screen, l retro.Layout) {
	t := f.t
	pb := t.Box("Progress")
	pb.SetRect(1, l.LowerY, l.LeftW, retro.LowerHeight)
	pb.Draw(screen)
	x, y, w, _ := pb.GetInnerRect()
	x, w = x+1, w-2
	step, name := "Summary", "Check and install"
	if f.step < stepSummary {
		step, name = fmt.Sprintf("Step %d of %d", f.step+1, len(stepTitles)), stepTitles[f.step]
	}
	pct := f.step * 100 / len(stepTitles)
	retro.LeftRight(screen, x, y, w, step, fmt.Sprintf("%d%%", pct), t.Base)
	t.Progress(screen, x, y+1, w, float64(pct)/100)
	retro.Center(screen, x, y+2, w, name, t.Base)
	retro.Center(screen, x, y+3, w, "Proxmox host setup", t.Base)
}

// drawField draws a text field after "> ". Empty, it shows the example in grey; a long
// value shows its end.
func (f *form) drawField(b *body, value, example string) {
	const prefix = "> "
	w := b.w - len(prefix) - 1
	shown := value
	if r := []rune(value); len(r) > w {
		more := "…"
		if f.t.ASCII {
			more = "<"
		}
		shown = more + string(r[len(r)-w+1:])
	}
	b.curRow, b.curCol = len(b.lines), len(prefix)+retro.Width(shown)
	if value == "" {
		b.add(true, seg{prefix, f.t.Base}, seg{retro.Cut(example, w), f.t.Example}, seg{"", f.t.Field})
	} else {
		b.add(true, seg{prefix, f.t.Base}, seg{shown, f.t.Field})
	}
}

// window returns the first visible row of a list of n rows with cursor cur, keeping the
// cursor inside h visible rows.
func window(top, cur, n, h int) int {
	h = max(h, 1)
	if cur < top {
		top = cur
	}
	if cur >= top+h {
		top = cur - h + 1
	}
	return max(min(top, n-h), 0)
}

// drawList draws a scrolling list with a header; row i is text(i).
func (f *form) drawList(b *body, header string, n, cur int, top *int, text func(i int) string) {
	h := b.bodyHeight - 3
	*top = window(*top, cur, n, h)
	b.add(false, seg{header, f.t.Strong})
	if *top > 0 {
		b.text(fmt.Sprintf("  (%d more above)", *top))
	} else {
		b.text("")
	}
	end := min(*top+max(h, 1), n)
	for i := *top; i < end; i++ {
		f.row(b, i == cur, text(i))
	}
	if end < n {
		b.text(fmt.Sprintf("  (%d more below)", n-end))
	}
}

func (f *form) isTicked(vmid string) bool {
	id, _ := strconv.Atoi(vmid)
	for _, t := range f.ticked {
		if t == id {
			return true
		}
	}
	return false
}

func (f *form) drawGuests(b *body) {
	nw := 4
	for _, g := range f.gs {
		nw = max(nw, retro.Width(g.name))
	}
	f.drawList(b, fmt.Sprintf("      %-9s  %-*s  %s", "NUMBER", nw, "NAME", "STATUS"), len(f.gs), f.gCur, &f.gTop,
		func(i int) string {
			g := f.gs[i]
			return fmt.Sprintf("%s %-9s  %-*s  %s", retro.Checkbox(f.isTicked(g.vmid)), g.vmid, nw, g.name, g.status)
		})
}

func (f *form) drawStorages(b *body) {
	nw := 4
	for _, st := range f.sts {
		nw = max(nw, retro.Width(st.name))
	}
	f.drawList(b, fmt.Sprintf("      %-*s  %-10s  %s", nw, "NAME", "TYPE", "FREE GiB"), len(f.sts), f.sCur, &f.sTop,
		func(i int) string {
			st := f.sts[i]
			return fmt.Sprintf("%s %-*s  %-10s  %8d", retro.Radio(i == f.sCur), nw, st.name, st.typ, st.freeGiB)
		})
}

// limitLines are the current limits in plain words.
func (f *form) limitLines() []string {
	return []string{
		fmt.Sprintf("%-30s %d GiB", "Start a backup only with", f.v.MinFreeGiB),
		fmt.Sprintf("%-30s %d GiB", "Stop a running backup below", f.v.AbortFreeGiB),
		fmt.Sprintf("%-30s %s MiB/s", "Read limit", mib(f.v.BwlimitKiB)),
		fmt.Sprintf("%-30s %d", "Backups at the same time", f.v.MaxWorkers),
	}
}

func (f *form) drawLimits(b *body) {
	if !f.editing {
		for _, l := range f.limitLines() {
			b.text("  " + l)
		}
		b.text("")
		for i, c := range limitChoices {
			f.row(b, i == f.limCur, c)
		}
		return
	}
	for i, fl := range limitFields {
		label := fmt.Sprintf("%s%-26s ", f.mark(i == f.field), fl.label)
		if i == f.field {
			b.curRow, b.curCol = len(b.lines), retro.Width(label)+retro.Width(f.lim[i])
			b.add(false, seg{label, f.t.Strong}, seg{f.lim[i] + " ", f.t.Field})
		} else {
			b.text(label + f.lim[i])
		}
	}
}

func (f *form) drawSummary(b *body) {
	names := map[string]string{}
	for _, g := range f.gs {
		names[g.vmid] = g.name
	}
	var guests []string
	for _, id := range f.v.VMIDs {
		s := strconv.Itoa(id)
		guests = append(guests, s+" ("+names[s]+")")
	}
	const lw = 20 // the label column
	key := f.v.Pubkey
	if w := b.w - lw; retro.Width(key) > w && w > 30 {
		key = key[:w-13] + "..." + key[len(key)-10:]
	}
	var rows []string
	row := func(label, value string) {
		lines := retro.Wrap(value, b.w-lw)
		if len(lines) == 0 {
			lines = []string{""}
		}
		for i, l := range lines {
			if i == 0 {
				rows = append(rows, fmt.Sprintf("%-18s  %s", label, l))
			} else {
				rows = append(rows, strings.Repeat(" ", lw)+l)
			}
		}
	}
	row("Backup machine", f.v.From)
	row("Public key", key)
	row("Guests", strings.Join(guests, ", "))
	row("Fleecing storage", f.v.Storage)
	row("Limits", fmt.Sprintf("start with %d GiB free", f.v.MinFreeGiB))
	row("", fmt.Sprintf("stop below %d GiB free", f.v.AbortFreeGiB))
	row("", fmt.Sprintf("read %s MiB/s, %d at a time", mib(f.v.BwlimitKiB), f.v.MaxWorkers))
	row("Backup user", f.v.User)
	// a blank line and the buttons always show; a summary longer than the box is cut short
	avail := b.bodyHeight - 2
	if len(rows) > avail {
		rows = append(rows[:max(avail-1, 0)], strings.Repeat(" ", lw)+"...")
	}
	b.text("")
	for _, r := range rows {
		b.text(r)
	}
	if len(rows) < avail { // a blank line above the buttons when there is room
		b.text("")
	}
	var segs []seg
	for i, label := range buttons {
		st := f.t.Base
		if i == f.button {
			st = f.t.Focus
		}
		segs = append(segs, seg{label, st})
		if i < len(buttons)-1 {
			segs = append(segs, seg{"  ", f.t.Base})
		}
	}
	b.add(false, segs...)
}
