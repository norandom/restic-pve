package install

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The examples shown in the form (grey, or as an "e.g." line with NO_COLOR) and in the plain
// questions. An example is never taken as a value.
const (
	exampleFrom = "192.0.2.10"
	exampleKey  = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI..."
)

// The smallest terminal the form is drawn on; a smaller one gets the plain questions.
const minCols, minRows = 60, 20

// Terminal is the terminal the form runs on: term_linux.go on a host, a fake in tests.
type Terminal interface {
	// Size returns the columns and rows.
	Size() (cols, rows int, err error)
	// Start switches to raw mode on the alternate screen. stop restores the terminal and may
	// be called more than once.
	Start() (stop func(), err error)
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
// before it returns, also on a panic.
func (s *System) form(def *Values, gs []guest, sts []storage) (*Values, error) {
	stop, err := s.Term.Start()
	if err != nil {
		return nil, errNoForm
	}
	defer stop()
	return newForm(def, gs, sts, s.Stdin, s.Stdout, s.termSize, s.Getenv("NO_COLOR") == "").run()
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
	kEOF
)

type keyIn struct {
	kind keyKind
	r    rune
}

// keyReader turns the bytes of a raw terminal into keys. "\r", "\n" and "\r\n" are one
// Enter. Inside a bracketed paste an Enter ends the paste: the rest of it is dropped.
type keyReader struct {
	r            io.Reader
	buf          []byte
	queue        []keyIn
	paste, skip  bool
	lastCR, done bool
}

func (k *keyReader) next() keyIn {
	for len(k.queue) == 0 {
		if k.done {
			return keyIn{kind: kEOF}
		}
		chunk := make([]byte, 4096)
		n, err := k.r.Read(chunk)
		k.buf = append(k.buf, chunk[:n]...)
		if err != nil {
			k.done = true
		}
		k.parse()
	}
	kk := k.queue[0]
	k.queue = k.queue[1:]
	return kk
}

func (k *keyReader) emit(kk keyIn) {
	if k.skip && kk.kind != kCtrlC {
		return
	}
	k.queue = append(k.queue, kk)
	if k.paste && kk.kind == kEnter {
		k.skip = true
	}
}

// parse moves the complete keys of buf to the queue; an incomplete escape sequence or
// UTF-8 character at the end waits for the next read.
func (k *keyReader) parse() {
	b := k.buf
	for len(b) > 0 {
		c := b[0]
		cr := false
		switch {
		case c == 0x1b && len(b) > 1 && (b[1] == '[' || b[1] == 'O'):
			j := 2
			for j < len(b) && (b[j] < 0x40 || b[j] > 0x7e) {
				j++
			}
			if j == len(b) && !k.done {
				k.buf = append([]byte(nil), b...)
				return
			}
			if j == len(b) {
				j--
			}
			k.sequence(string(b[1 : j+1]))
			b = b[j+1:]
			k.lastCR = false
			continue
		case c == 0x1b:
			k.emit(keyIn{kind: kEsc})
		case c == '\r':
			k.emit(keyIn{kind: kEnter})
			cr = true
		case c == '\n':
			if !k.lastCR {
				k.emit(keyIn{kind: kEnter})
			}
		case c == 0x03:
			k.emit(keyIn{kind: kCtrlC})
		case c == 0x7f || c == 0x08:
			k.emit(keyIn{kind: kBackspace})
		case c == '\t':
			k.emit(keyIn{kind: kTab})
		case c == 0x15:
			k.emit(keyIn{kind: kCtrlU})
		case c < 0x20:
		default:
			if !utf8.FullRune(b) && !k.done {
				k.buf = append([]byte(nil), b...)
				return
			}
			r, n := utf8.DecodeRune(b)
			if unicode.IsPrint(r) {
				k.emit(keyIn{kind: kRune, r: r})
			}
			b = b[n:]
			k.lastCR = false
			continue
		}
		b = b[1:]
		k.lastCR = cr
	}
	k.buf = nil
}

// sequence handles one escape sequence without its ESC.
func (k *keyReader) sequence(seq string) {
	switch seq {
	case "[A", "OA":
		k.emit(keyIn{kind: kUp})
	case "[B", "OB":
		k.emit(keyIn{kind: kDown})
	case "[C", "OC":
		k.emit(keyIn{kind: kRight})
	case "[D", "OD":
		k.emit(keyIn{kind: kLeft})
	case "[Z":
		k.emit(keyIn{kind: kBackTab})
	case "[200~":
		k.paste = true
	case "[201~":
		k.paste, k.skip = false, false
	}
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

// The four limit fields of "Change": label and one sentence with the allowed range.
var limitFields = []struct{ label, help string }{
	{"Free space to start (GiB)", "A backup starts only when the fleecing storage has this much free space. 1-999999."},
	{"Free space to stop (GiB)", "A running backup stops when free space falls below this. Lower than the start value, 1-999999."},
	{"Read limit (MiB/s)", "How fast a backup may read the disks of a guest. 1-" + strconv.Itoa(maxBwlimitMiB) + "."},
	{"Backups at the same time", "How many guests are backed up at once. 1-16."},
}

var (
	limitChoices = []string{"Keep these (recommended)", "Change"}
	buttons      = []string{"[ Install ]", "[ Back ]", "[ Cancel ]"}
)

const (
	footerText    = "Enter continue · Esc back · Ctrl-U clear · Ctrl-C cancel"
	footerTicks   = "Up/Down move · Space tick · Enter continue · Esc back · Ctrl-C cancel"
	footerChoice  = "Up/Down move · Enter choose · Esc back · Ctrl-C cancel"
	footerFields  = "Up/Down field · Enter next · Esc back · Ctrl-U clear · Ctrl-C cancel"
	footerSummary = "Left/Right move · Enter press · Esc back · Ctrl-C cancel"
)

type form struct {
	keys   *keyReader
	out    io.Writer
	size   func() (int, int)
	color  bool
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
}

func newForm(def *Values, gs []guest, sts []storage, in io.Reader, out io.Writer, size func() (int, int), color bool) *form {
	f := &form{keys: &keyReader{r: in}, out: out, size: size, color: color, v: *def, gs: gs, sts: sts,
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

func (f *form) run() (*Values, error) {
	for {
		if len(f.keys.queue) == 0 { // a paste is drawn once, not once per character
			f.draw()
		}
		k := f.keys.next()
		switch k.kind {
		case kCtrlC:
			return nil, errStop("cancelled")
		case kEOF:
			return nil, errStop("the terminal closed")
		}
		done, err := f.handle(k)
		if err != nil {
			return nil, err
		}
		if done {
			v := f.v
			return &v, nil
		}
	}
}

func (f *form) handle(k keyIn) (bool, error) {
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

// screen collects the lines of one frame and the place of the text cursor.
type screen struct {
	cols       int
	lines      []string
	curRow     int // 1-based; 0 hides the cursor
	curCol     int
	bodyHeight int
}

func (f *form) sgr(code, s string) string {
	if code == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// colour is sgr for colours only, which NO_COLOR turns off.
func (f *form) colour(code, s string) string {
	if !f.color {
		return s
	}
	return f.sgr(code, s)
}

// cut shortens s to at most w characters.
func cut(s string, w int) string {
	if r := []rune(s); len(r) > w {
		if w < 1 {
			return ""
		}
		return string(r[:w])
	}
	return s
}

// wrap breaks text into lines of at most w characters at spaces.
func wrap(text string, w int) []string {
	var lines []string
	line := ""
	for _, word := range strings.Fields(text) {
		switch {
		case line == "":
			line = word
		case utf8.RuneCountInString(line)+1+utf8.RuneCountInString(word) <= w:
			line += " " + word
		default:
			lines = append(lines, line)
			line = word
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

// add appends a line: the plain text plain (cut to the width) shown with styled.
func (sc *screen) add(plain string, style func(string) string) {
	t := cut(plain, sc.cols-1)
	if style != nil {
		t = style(t)
	}
	sc.lines = append(sc.lines, t)
}

func (sc *screen) text(s string) { sc.add(s, nil) }

func (sc *screen) para(indent, text string) {
	for _, l := range wrap(text, sc.cols-1-len(indent)) {
		sc.text(indent + l)
	}
}

func (f *form) draw() {
	cols, rows := f.size()
	sc := &screen{cols: cols}
	title := "restic-pve install · "
	if f.step < stepSummary {
		title += fmt.Sprintf("step %d of %d: %s", f.step+1, len(stepTitles), stepTitles[f.step])
	} else {
		title += "summary"
	}
	sc.add(title, func(s string) string { return f.sgr("1", s) })
	sc.text("")
	footer := footerChoice
	switch f.step {
	case stepFrom, stepKey:
		footer = footerText
	case stepGuests:
		footer = footerTicks
	case stepLimits:
		if f.editing {
			footer = footerFields
		}
	case stepSummary:
		footer = footerSummary
	}
	foot := wrap(footer, cols-1)
	if f.step < stepSummary {
		sc.para("", stepTexts[f.step])
	} else {
		sc.para("", summaryText)
	}
	sc.text("")
	sc.bodyHeight = rows - len(sc.lines) - 2 - len(foot) // a blank line, the message, the footer
	top := len(sc.lines)
	switch f.step {
	case stepFrom:
		f.drawField(sc, f.from, exampleFrom)
	case stepKey:
		f.drawField(sc, f.key, exampleKey)
	case stepGuests:
		f.drawGuests(sc)
	case stepStorage:
		f.drawStorages(sc)
	case stepLimits:
		f.drawLimits(sc)
	case stepSummary:
		f.drawSummary(sc)
	}
	if len(sc.lines) > top+sc.bodyHeight {
		sc.lines = sc.lines[:top+max(sc.bodyHeight, 0)]
	}
	for len(sc.lines) < rows-1-len(foot) {
		sc.text("")
	}
	sc.add(f.msg, func(s string) string { return f.colour("31", s) })
	for _, l := range foot {
		sc.add(l, func(s string) string { return f.colour("2", s) })
	}

	var b bytes.Buffer
	b.WriteString("\x1b[?25l\x1b[H")
	for i, l := range sc.lines {
		if i >= rows {
			break
		}
		b.WriteString(l + "\x1b[K")
		if i < rows-1 && i < len(sc.lines)-1 {
			b.WriteString("\r\n")
		}
	}
	b.WriteString("\x1b[J")
	if sc.curRow > 0 && sc.curRow <= rows {
		fmt.Fprintf(&b, "\x1b[%d;%dH\x1b[?25h", sc.curRow, sc.curCol)
	}
	_, _ = f.out.Write(b.Bytes())
}

// drawField draws a text field. Empty, it shows the example in grey; a long value shows its
// end. With NO_COLOR the example is a line of its own.
func (f *form) drawField(sc *screen, value, example string) {
	const prefix = "  > "
	w := sc.cols - 1 - len(prefix) - 1
	shown := value
	if r := []rune(value); len(r) > w {
		shown = "…" + string(r[len(r)-w+1:])
	}
	sc.curRow, sc.curCol = len(sc.lines)+1, len(prefix)+utf8.RuneCountInString(shown)+1
	if value == "" && f.color {
		sc.lines = append(sc.lines, prefix+f.sgr("2", cut(example, w)))
	} else {
		sc.text(prefix + shown)
	}
	if !f.color {
		sc.text("")
		sc.text("    e.g. " + example)
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

// drawList draws a scrolling list with a header; row i is text(i), the cursor row has ">".
// below rows of the body are kept free for lines after the list.
func (f *form) drawList(sc *screen, below int, header string, n, cur int, top *int, text func(i int) string) {
	h := sc.bodyHeight - 3 - below
	*top = window(*top, cur, n, h)
	sc.add("    "+header, func(s string) string { return f.sgr("1", s) })
	if *top > 0 {
		sc.text(fmt.Sprintf("    (%d more above)", *top))
	} else {
		sc.text("")
	}
	end := min(*top+max(h, 1), n)
	for i := *top; i < end; i++ {
		if i == cur {
			sc.add("  > "+text(i), func(s string) string { return f.sgr("7", s) })
		} else {
			sc.text("    " + text(i))
		}
	}
	if end < n {
		sc.text(fmt.Sprintf("    (%d more below)", n-end))
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

func (f *form) drawGuests(sc *screen) {
	nw := 4
	for _, g := range f.gs {
		nw = max(nw, utf8.RuneCountInString(g.name))
	}
	f.drawList(sc, 2, fmt.Sprintf("    %-9s  %-*s  %s", "NUMBER", nw, "NAME", "STATUS"), len(f.gs), f.gCur, &f.gTop,
		func(i int) string {
			g := f.gs[i]
			box := "[ ]"
			if f.isTicked(g.vmid) {
				box = "[x]"
			}
			return fmt.Sprintf("%s %-9s  %-*s  %s", box, g.vmid, nw, g.name, g.status)
		})
	sc.text("")
	sc.text(fmt.Sprintf("  %d ticked", len(f.ticked)))
}

func (f *form) drawStorages(sc *screen) {
	nw := 4
	for _, st := range f.sts {
		nw = max(nw, utf8.RuneCountInString(st.name))
	}
	f.drawList(sc, 0, fmt.Sprintf("    %-*s  %-10s  %s", nw, "NAME", "TYPE", "FREE GiB"), len(f.sts), f.sCur, &f.sTop,
		func(i int) string {
			st := f.sts[i]
			mark := "( )"
			if i == f.sCur {
				mark = "(*)"
			}
			return fmt.Sprintf("%s %-*s  %-10s  %8d", mark, nw, st.name, st.typ, st.freeGiB)
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

func (f *form) drawLimits(sc *screen) {
	if !f.editing {
		for _, l := range f.limitLines() {
			sc.text("    " + l)
		}
		sc.text("")
		for i, c := range limitChoices {
			if i == f.limCur {
				sc.add("  > "+c, func(s string) string { return f.sgr("7", s) })
			} else {
				sc.text("    " + c)
			}
		}
		return
	}
	// all four sentences when they fit, else only the one of the field being edited
	all := len(limitFields)
	for _, fl := range limitFields {
		all += len(wrap(fl.help, sc.cols-1-6))
	}
	for i, fl := range limitFields {
		prefix := "    "
		if i == f.field {
			prefix = "  > "
		}
		line := fmt.Sprintf("%s%-26s %s", prefix, fl.label, f.lim[i])
		if i == f.field {
			sc.curRow, sc.curCol = len(sc.lines)+1, utf8.RuneCountInString(cut(line, sc.cols-1))+1
		}
		sc.text(line)
		if all <= sc.bodyHeight || i == f.field {
			sc.para("      ", fl.help)
		}
	}
}

func (f *form) drawSummary(sc *screen) {
	names := map[string]string{}
	for _, g := range f.gs {
		names[g.vmid] = g.name
	}
	var guests []string
	for _, id := range f.v.VMIDs {
		s := strconv.Itoa(id)
		guests = append(guests, s+" ("+names[s]+")")
	}
	key := f.v.Pubkey
	if w := sc.cols - 1 - 22; utf8.RuneCountInString(key) > w && w > 30 {
		key = key[:w-13] + "..." + key[len(key)-10:]
	}
	var rows []string
	row := func(label, value string) {
		lines := wrap(value, sc.cols-1-22)
		if len(lines) == 0 {
			lines = []string{""}
		}
		for i, l := range lines {
			if i == 0 {
				rows = append(rows, fmt.Sprintf("  %-18s  %s", label, l))
			} else {
				rows = append(rows, strings.Repeat(" ", 22)+l)
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
	// the buttons always show; a summary longer than the terminal is cut short
	if avail := sc.bodyHeight - 2; len(rows) > avail {
		rows = append(rows[:max(avail-1, 0)], strings.Repeat(" ", 22)+"...")
	}
	for _, r := range rows {
		sc.text(r)
	}
	sc.text("")
	line := "  "
	for i, b := range buttons {
		if i == f.button {
			line += f.sgr("7", b)
		} else {
			line += b
		}
		if i < len(buttons)-1 {
			line += "  "
		}
	}
	sc.lines = append(sc.lines, line)
}
