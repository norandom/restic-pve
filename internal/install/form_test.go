package install

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/norandom/restic-pve/internal/retro"
)

// shot is one screen the form showed: its text, one line per row, and its cells.
type shot struct {
	text       string
	cells      []tcell.SimCell
	cols, rows int
	cursor     bool
}

// line returns row y of the shot.
func (s shot) line(y int) string { return strings.Split(s.text, "\n")[y] }

// styleOf returns the style of the first cell of the first occurrence of want.
func (s shot) styleOf(t *testing.T, want string) tcell.Style {
	t.Helper()
	for y, l := range strings.Split(s.text, "\n") {
		if i := strings.Index(l, want); i >= 0 {
			x := len([]rune(l[:i]))
			return s.cells[y*s.cols+x].Style
		}
	}
	t.Fatalf("%q not on the screen:\n%s", want, s.text)
	return tcell.StyleDefault
}

// region returns the text inside a part of the screen, its lines joined by spaces: prose
// wraps anywhere.
func (s shot) region(x, y, w, h int) string {
	var parts []string
	lines := strings.Split(s.text, "\n")
	for row := y; row < y+h && row < len(lines); row++ {
		r := []rune(lines[row])
		parts = append(parts, strings.TrimSpace(string(r[min(x, len(r)):min(x+w, len(r))])))
	}
	return strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
}

// help is the text of the Help box.
func (s shot) help() string {
	l := retro.NewLayout(s.cols, s.rows)
	return s.region(l.RightX+1, l.LowerY+1, l.RightW()-2, retro.LowerHeight-2)
}

// main is the text of the main box.
func (s shot) main() string {
	l := retro.NewLayout(s.cols, s.rows)
	return s.region(2, l.BodyY+1, s.cols-4, l.BodyH-2)
}

// fakeTerm is the terminal of the form tests: a size, a simulated screen that takes the
// keys and records every screen shown, and counters for the start and the restore.
type fakeTerm struct {
	cols, rows       int
	charset          string
	sizeErr, failure error
	started, stopped int
	panicAfterStart  bool

	keys  []string
	shots []shot
}

func (t *fakeTerm) Size() (int, int, error) { return t.cols, t.rows, t.sizeErr }

func (t *fakeTerm) Screen() (tcell.Screen, error) {
	if t.failure != nil {
		return nil, t.failure
	}
	return &recScreen{SimulationScreen: tcell.NewSimulationScreen(t.charset), t: t}, nil
}

func (t *fakeTerm) Watch(func()) func() { return func() {} }

// recScreen is tcell's simulation screen that gives the test keys as events, then the end
// of the terminal, and records what it shows.
type recScreen struct {
	tcell.SimulationScreen
	t    *fakeTerm
	mu   sync.Mutex
	evs  []tcell.Event
	fed  bool
	fini bool
}

func (s *recScreen) Init() error {
	if err := s.SimulationScreen.Init(); err != nil {
		return err
	}
	s.SetSize(s.t.cols, s.t.rows)
	s.t.started++
	return nil
}

func (s *recScreen) Fini() {
	s.mu.Lock()
	first := !s.fini
	s.fini = true
	s.mu.Unlock()
	if first {
		s.t.stopped++
		s.SimulationScreen.Fini()
	}
}

func (s *recScreen) Show() {
	if s.t.panicAfterStart {
		panic("show")
	}
	s.SimulationScreen.Show()
	cells, w, h := s.GetContents()
	var b strings.Builder
	for y := 0; y < h; y++ {
		if y > 0 {
			b.WriteByte('\n')
		}
		for x := 0; x < w; x++ {
			c := cells[y*w+x]
			if len(c.Runes) == 0 {
				b.WriteByte(' ')
			} else {
				b.WriteRune(c.Runes[0])
			}
		}
	}
	_, _, vis := s.GetCursor()
	s.t.shots = append(s.t.shots, shot{text: b.String(), cells: append([]tcell.SimCell(nil), cells...), cols: w, rows: h, cursor: vis})
}

func (s *recScreen) PollEvent() tcell.Event {
	s.mu.Lock()
	if s.fini {
		s.mu.Unlock()
		return nil
	}
	if !s.fed {
		s.fed = true
		for _, k := range s.t.keys {
			s.evs = append(s.evs, events(k)...)
		}
		s.evs = append(s.evs, tcell.NewEventError(errors.New("end of keys")))
	}
	if len(s.evs) == 0 {
		s.mu.Unlock()
		return s.SimulationScreen.PollEvent()
	}
	ev := s.evs[0]
	s.evs = s.evs[1:]
	s.mu.Unlock()
	return ev
}

// The keys as a terminal sends them.
const (
	enterKey = "\r"
	esc      = "\x1b"
	up       = "\x1b[A"
	down     = "\x1b[B"
	right    = "\x1b[C"
	left     = "\x1b[D"
	tab      = "\t"
	btab     = "\x1b[Z"
	ctrlC    = "\x03"
	ctrlU    = "\x15"
	bs       = "\x7f"
	pasteA   = "\x1b[200~"
	pasteZ   = "\x1b[201~"
)

// events turns one key string of a test into the events tcell gives for it.
func events(k string) []tcell.Event {
	ev := func(key tcell.Key, r rune) tcell.Event { return tcell.NewEventKey(key, r, tcell.ModNone) }
	switch k {
	case esc:
		return []tcell.Event{ev(tcell.KeyEsc, 0)}
	case up:
		return []tcell.Event{ev(tcell.KeyUp, 0)}
	case down:
		return []tcell.Event{ev(tcell.KeyDown, 0)}
	case right:
		return []tcell.Event{ev(tcell.KeyRight, 0)}
	case left:
		return []tcell.Event{ev(tcell.KeyLeft, 0)}
	case btab:
		return []tcell.Event{ev(tcell.KeyBacktab, 0)}
	}
	var out []tcell.Event
	paste := strings.HasPrefix(k, pasteA) && strings.HasSuffix(k, pasteZ)
	if paste {
		out = append(out, tcell.NewEventPaste(true))
		k = strings.TrimSuffix(strings.TrimPrefix(k, pasteA), pasteZ)
	}
	for _, r := range k {
		switch r {
		case '\r':
			out = append(out, ev(tcell.KeyEnter, 0))
		case '\t':
			out = append(out, ev(tcell.KeyTab, 0))
		case 0x03:
			out = append(out, ev(tcell.KeyCtrlC, 0))
		case 0x15:
			out = append(out, ev(tcell.KeyCtrlU, 0))
		case 0x7f:
			out = append(out, ev(tcell.KeyBackspace2, 0))
		default:
			out = append(out, ev(tcell.KeyRune, r))
		}
	}
	if paste {
		out = append(out, tcell.NewEventPaste(false))
	}
	return out
}

// formHost is a fake host whose stdin and stdout are an 80x24 UTF-8 terminal.
func formHost(t *testing.T, env ...string) (*fakeHost, *fakeTerm) {
	h := newHost(t)
	term := &fakeTerm{cols: 80, rows: 24, charset: "UTF-8"}
	vars := map[string]string{"TERM": "xterm-256color"}
	for i := 0; i+1 < len(env); i += 2 {
		vars[env[i]] = env[i+1]
	}
	h.sys.Term, h.sys.Getenv = term, func(k string) string { return vars[k] }
	return h, term
}

// form runs install with the keys.
func (h *fakeHost) form(keys ...string) int {
	h.out.Reset()
	h.errb.Reset()
	h.cmds = nil
	term := h.sys.Term.(*fakeTerm)
	term.keys, term.shots = keys, nil
	h.sys.Stdin = bufio.NewReader(strings.NewReader(""))
	return h.sys.Install()
}

// shots returns the screens of the last run.
func (h *fakeHost) shots() []shot { return h.sys.Term.(*fakeTerm).shots }

// screenOf returns the first screen that contains want, also as wrapped text in the Help box.
func screenOf(t *testing.T, h *fakeHost, want string) shot {
	t.Helper()
	for _, s := range h.shots() {
		if strings.Contains(s.text, want) || strings.Contains(s.help(), want) {
			return s
		}
	}
	t.Fatalf("no screen with %q; last:\n%s", want, last(h).text)
	return shot{}
}

// lastOf returns the last screen that contains want.
func lastOf(t *testing.T, h *fakeHost, want string) shot {
	t.Helper()
	ss := h.shots()
	for i := len(ss) - 1; i >= 0; i-- {
		if strings.Contains(ss[i].text, want) {
			return ss[i]
		}
	}
	t.Fatalf("no screen with %q", want)
	return shot{}
}

// last returns the last screen before the form closed.
func last(h *fakeHost) shot {
	ss := h.shots()
	if len(ss) == 0 {
		return shot{}
	}
	return ss[len(ss)-1]
}

func through(t *testing.T, label string, code int, h *fakeHost) {
	t.Helper()
	if code != 0 {
		t.Fatalf("%s: %d %s\n%s\n%s", label, code, h.errb.String(), h.out.String(), last(h).text)
	}
}

func TestFormFreshInstall(t *testing.T) {
	h, term := formHost(t)
	// the paste ends with Enter; the space after it in the same paste is dropped
	code := h.form("192.0.2.10", enterKey, pasteA+key+" restic-backup@backup.example\r "+pasteZ,
		" ", "j", " ", enterKey, enterKey, enterKey, enterKey)
	through(t, "fresh", code, h)
	if term.started != 1 || term.stopped != 1 {
		t.Fatalf("terminal started %d stopped %d", term.started, term.stopped)
	}
	if text, _ := h.file(t, confFile); text != confText {
		t.Fatalf("conf %q", text)
	}
	if text, _ := h.file(t, "/home/restic-backup/.ssh/authorized_keys"); text != `restrict,from="192.0.2.10",command="sudo -n /usr/local/sbin/restic-pve request $SSH_ORIGINAL_COMMAND" `+key+"\n" {
		t.Fatalf("authorized_keys %q", text)
	}
	out := h.out.String()
	if !strings.Contains(out, "written   /etc/restic-pve.conf\n") || !strings.HasSuffix(out, "ready: user restic-backup, key accepted from 192.0.2.10, guests: 100 101\n"+
		"This host updates restic-pve daily to the latest signed release (restic-pve-update.timer).\n") {
		t.Fatalf("progress lines missing after the form:\n%s", out)
	}
	if strings.Contains(out, "Enter takes the value in [brackets]") || strings.Contains(out, "\x1b[") {
		t.Fatal("plain questions or escape sequences on stdout")
	}
}

// The visible texts of every screen, once, and the retro look: frame, bars, boxes, colours.
func TestFormScreens(t *testing.T) {
	h, _ := formHost(t)
	through(t, "screens", h.form("192.0.2.10", enterKey, key, enterKey, " ", enterKey, down, enterKey, enterKey, enterKey), h)
	cases := []struct {
		title, box string
		main, help []string
		keys       string
	}{
		{"restic-pve install · step 1 of 5: Backup machine address", "─ Backup machine address ─", []string{
			"Type the IP address of the backup machine", "Only this address may log in with the backup key.",
			"Run ./setup.sh --proxmox on the backup machine; it prints this address.", "> 192.0.2.10"},
			[]string{"Example: 192.0.2.10"}, "Enter continue · Esc back · Ctrl-U clear"},
		{"restic-pve install · step 2 of 5: Public key", "─ Public key ─", []string{
			"Paste the whole public key line of the backup machine. It starts with ssh-ed25519",
			"A public key is not a secret.", "> ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI..."},
			[]string{"Example: ssh-ed25519 AAAA..."}, "Enter continue · Esc back · Ctrl-U clear"},
		{"restic-pve install · step 3 of 5: Guests", "─ Guests ─", []string{
			"Tick the virtual machines to back up.", "NUMBER NAME STATUS",
			"► [ ] 100 web running", "[ ] 102 scratch stopped"},
			[]string{"0 ticked"}, "Up/Down move · Space tick · Enter continue · Esc back"},
		{"restic-pve install · step 4 of 5: Fleecing storage", "─ Fleecing storage ─", []string{
			"temporary data while a running guest is read, so the guest", "most free space on fast disks",
			"NAME TYPE FREE GiB", "► (*) fast-ssd lvmthin 819", "( ) local dir 77"},
			[]string{"fast-ssd: lvmthin, 819 GiB free"}, "Up/Down move · Enter choose · Esc back"},
		{"restic-pve install · step 5 of 5: Limits", "─ Limits ─", []string{
			"These limits protect this host while a backup runs.",
			"Start a backup only with 32 GiB", "Stop a running backup below 8 GiB",
			"Read limit 40 MiB/s", "Backups at the same time 1",
			"► Keep these (recommended)", "Change"},
			[]string{"Keep: the values above stay."}, "Up/Down move · Enter choose · Esc back"},
		{"restic-pve install · summary", "─ Summary ─", []string{
			"a backup user with its key, a sudoers file, the binary and a daily update timer. Nothing changes before you choose Install.",
			"Backup machine 192.0.2.10", "Public key ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEFB", "Guests 100 (web)",
			"Fleecing storage local", "Limits start with 32 GiB free stop below 8 GiB free read 40 MiB/s, 1 at a time",
			"Backup user restic-backup", "[ Install ] [ Back ] [ Cancel ]"},
			[]string{"Cancel changes nothing."}, "Left/Right move · Enter press · Esc back"},
	}
	for _, c := range cases {
		s := screenOf(t, h, c.title)
		if s.rows != 24 || s.cols != 80 {
			t.Errorf("%s: %dx%d", c.title, s.cols, s.rows)
		}
		if !strings.HasPrefix(s.line(0), " "+c.title) || !strings.HasSuffix(s.line(0), " Ctrl-C=Cancel ") {
			t.Errorf("title bar %q", s.line(0))
		}
		if !strings.HasPrefix(s.line(1), " ┌──") || !strings.Contains(s.line(1), c.box) || !strings.HasPrefix(s.line(17), " ┌") ||
			!strings.Contains(s.line(17), " Progress ") || !strings.Contains(s.line(17), "┐ ┌") ||
			!strings.Contains(s.line(17), " Help ") || !strings.HasPrefix(s.line(23), c.keys) ||
			!strings.HasSuffix(s.line(23), "│ restic-pve ") {
			t.Errorf("%s: layout:\n%s", c.title, s.text)
		}
		for _, w := range c.main {
			if !strings.Contains(s.main(), w) {
				t.Errorf("%s: main box lacks %q:\n%s", c.title, w, s.text)
			}
		}
		for _, w := range c.help {
			if !strings.Contains(s.help(), w) {
				t.Errorf("%s: help lacks %q: %q", c.title, w, s.help())
			}
		}
	}
	// the colours of the house style
	s := screenOf(t, h, "step 1 of 5")
	blue := tcell.NewRGBColor(0x55, 0x55, 0xff)
	if fg, bg, _ := s.styleOf(t, "Type the IP").Decompose(); bg != blue || fg != tcell.NewRGBColor(0xff, 0xff, 0xff) {
		t.Errorf("text %v on %v", fg, bg)
	}
	if fg, _, _ := s.styleOf(t, "Backup machine address ─").Decompose(); fg != tcell.NewRGBColor(0xff, 0xff, 0x55) {
		t.Errorf("box title %v", fg)
	}
	if fg, bg, _ := s.styleOf(t, " restic-pve install").Decompose(); bg != tcell.NewRGBColor(0xaa, 0xaa, 0xaa) || fg != tcell.NewRGBColor(0, 0, 0) {
		t.Errorf("title bar %v on %v", fg, bg)
	}
	if fg, _, _ := s.styleOf(t, "192.0.2.10").Decompose(); fg != tcell.NewRGBColor(0xaa, 0xaa, 0xaa) {
		t.Errorf("example not grey: %v", fg)
	}
	if !s.cursor {
		t.Error("no cursor in the text field")
	}
	s = screenOf(t, h, "summary")
	if _, bg, _ := s.styleOf(t, "[ Install ]").Decompose(); bg != tcell.NewRGBColor(0xff, 0xff, 0x55) {
		t.Errorf("Install not focused: %v", bg)
	}
	if _, bg, _ := s.styleOf(t, "[ Back ]").Decompose(); bg != blue {
		t.Errorf("Back focused: %v", bg)
	}
	s = screenOf(t, h, "step 5 of 5: Limits")
	if s.cursor {
		t.Error("cursor shown on a list")
	}
	if _, bg, _ := s.styleOf(t, "► Keep these").Decompose(); bg != tcell.NewRGBColor(0xff, 0xff, 0x55) {
		t.Errorf("focused row not yellow: %v", bg)
	}
}

func TestFormExampleIsNeverTheValue(t *testing.T) {
	h, term := formHost(t)
	code := h.form(enterKey, "192.0.2.10", enterKey, enterKey, ctrlC)
	h.stopped(t, "examples", code, "cancelled")
	s := screenOf(t, h, "an address or pattern for authorized_keys from=")
	if !strings.Contains(s.text, "step 1 of 5") {
		t.Fatalf("left the address screen:\n%s", s.text)
	}
	if _, bg, _ := s.styleOf(t, "an address").Decompose(); bg != tcell.NewRGBColor(0xaa, 0, 0) {
		t.Errorf("rule not in the error style: %v", bg)
	}
	s = screenOf(t, h, "one line 'ssh-ed25519 <base64> [comment]'")
	if !strings.Contains(s.text, "step 2 of 5") {
		t.Fatalf("left the key screen:\n%s", s.text)
	}
	if term.stopped != 1 {
		t.Fatal("terminal not restored")
	}
}

func TestFormInvalidValueStays(t *testing.T) {
	cases := []struct {
		keys []string
		rule string
		step string
	}{
		{[]string{"192.0.2.10 x", enterKey}, "an address or pattern for authorized_keys from=", "step 1 of 5"},
		{[]string{`a"b`, enterKey}, "an address or pattern", "step 1 of 5"},
		{[]string{"192.0.2.10", enterKey, "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQ", enterKey}, "one line 'ssh-ed25519", "step 2 of 5"},
		{[]string{"192.0.2.10", enterKey, key + "x", enterKey}, "one line 'ssh-ed25519", "step 2 of 5"},
		{[]string{"192.0.2.10", enterKey, key, enterKey, enterKey}, "give at least one VM number", "step 3 of 5"},
	}
	for _, c := range cases {
		h, _ := formHost(t)
		h.stopped(t, c.rule, h.form(append(c.keys, ctrlC)...), "cancelled")
		s := last(h)
		if !strings.Contains(s.help(), c.rule) || !strings.Contains(s.text, c.step) {
			t.Errorf("%q: rule or step missing:\n%s", c.keys, s.text)
		}
	}
	// a wrong value is fixed in place: Backspace and Ctrl-U, then the install goes through
	h, _ := formHost(t)
	through(t, "fixed", h.form("192.0.2.10 x", enterKey, bs, bs, enterKey, "nonsense", enterKey, ctrlU, key, enterKey,
		" ", enterKey, enterKey, enterKey, enterKey), h)
	if text, _ := h.file(t, confFile); !strings.Contains(text, "VMIDS=100\n") {
		t.Fatal(text)
	}
}

func TestFormGuestListScrolls(t *testing.T) {
	h, term := formHost(t)
	term.rows = 20
	var qm strings.Builder
	qm.WriteString("      VMID NAME                 STATUS     MEM(MB)    BOOTDISK(GB) PID\n")
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&qm, "       %d guest-%02d             stopped    1024              16.00 0\n", 100+i, i)
	}
	h.qm = qm.String()
	keys := []string{"192.0.2.10", enterKey, key, enterKey}
	for i := 0; i < 15; i++ {
		keys = append(keys, down)
	}
	keys = append(keys, " ")
	for i := 0; i < 15; i++ {
		keys = append(keys, "k")
	}
	keys = append(keys, " ", enterKey, enterKey, enterKey, enterKey)
	through(t, "scroll", h.form(keys...), h)
	if text, _ := h.file(t, confFile); !strings.Contains(text, "VMIDS=115 100\n") {
		t.Fatalf("tick order not kept: %s", text)
	}
	s := screenOf(t, h, "► [x] 115")
	if !strings.Contains(s.text, "more above)") || strings.Contains(s.text, "guest-00") || s.rows != 20 {
		t.Fatalf("list did not scroll:\n%s", s.text)
	}
	if !strings.Contains(s.help(), "1 ticked") {
		t.Errorf("ticked count: %q", s.help())
	}
	s = screenOf(t, h, "step 3 of 5")
	if !strings.Contains(s.text, "more below)") || !strings.Contains(s.text, "guest-00") {
		t.Fatalf("first guest screen:\n%s", s.text)
	}
	// Space unticks again
	h, _ = formHost(t)
	through(t, "untick", h.form("192.0.2.10", enterKey, key, enterKey, " ", "j", " ", "k", " ", enterKey, enterKey, enterKey, enterKey), h)
	if text, _ := h.file(t, confFile); !strings.Contains(text, "VMIDS=101\n") {
		t.Fatal(text)
	}
}

func TestFormStorageChoice(t *testing.T) {
	h, _ := formHost(t)
	through(t, "storage", h.form("192.0.2.10", enterKey, key, enterKey, " ", enterKey, down, down, up, "j", enterKey, enterKey, enterKey), h)
	if text, _ := h.file(t, confFile); !strings.Contains(text, "FLEECING_STORAGE=local\n") {
		t.Fatal(text)
	}
}

func TestFormLimits(t *testing.T) {
	// Keep is the default and writes the current limits
	h, _ := formHost(t)
	through(t, "keep", h.form("192.0.2.10", enterKey, key, enterKey, " ", "j", " ", enterKey, enterKey, enterKey, enterKey), h)
	if text, _ := h.file(t, confFile); text != confText {
		t.Fatal(text)
	}
	// Change: four prefilled fields; a stop value not below the start value shows its rule
	h, _ = formHost(t)
	through(t, "change", h.form("192.0.2.10", enterKey, key, enterKey, " ", enterKey, enterKey, down, enterKey,
		ctrlU, "64", enterKey, ctrlU, "64", enterKey, ctrlU, "16", enterKey, bs, bs, "20", enterKey, ctrlU, "2", enterKey, enterKey), h)
	s := screenOf(t, h, "FLEECING_ABORT_FREE_GIB must be below FLEECING_MIN_FREE_GIB (64)")
	if !strings.Contains(s.text, "► Free space to stop (GiB)   64") {
		t.Fatalf("not on the stop field:\n%s", s.text)
	}
	s = screenOf(t, h, "Up/Down field")
	for _, w := range []string{"► Free space to start (GiB)  32", "  Free space to stop (GiB)   8", "  Read limit (MiB/s)         40",
		"  Backups at the same time   1"} {
		if !strings.Contains(s.text, w) {
			t.Errorf("fields lack %q:\n%s", w, s.text)
		}
	}
	if !strings.Contains(s.help(), "A backup starts only when the fleecing storage has this much free space. 1-999999.") {
		t.Errorf("help of the field: %q", s.help())
	}
	if !s.cursor {
		t.Error("no cursor in the field")
	}
	for i, w := range []string{"start value, 1-999999.", "1-976562.", "How many guests are backed up at once. 1-16."} {
		found := false
		for _, s := range h.shots() {
			if strings.Contains(s.help(), w) && strings.Contains(s.text, "► "+limitFields[i+1].label) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no help %q on field %d", w, i+1)
		}
	}
	if text, _ := h.file(t, confFile); !strings.Contains(text, "FLEECING_MIN_FREE_GIB=64\nFLEECING_ABORT_FREE_GIB=16\nBWLIMIT_KIB=20480\nMAX_WORKERS=2\nVMIDS=100\n") {
		t.Fatal(text)
	}
	// invalid numbers stay on their field
	for _, c := range []struct{ keys []string }{
		{[]string{ctrlU, "0", enterKey}},
		{[]string{enterKey, enterKey, ctrlU, "1.5", enterKey}},
		{[]string{enterKey, enterKey, enterKey, ctrlU, "17", enterKey}},
	} {
		h, _ := formHost(t)
		keys := append([]string{"192.0.2.10", enterKey, key, enterKey, " ", enterKey, enterKey, down, enterKey}, c.keys...)
		h.stopped(t, "limit", h.form(append(keys, ctrlC)...), "cancelled")
		s := last(h)
		if !strings.Contains(s.help(), "must be") || !strings.Contains(s.text, "Up/Down field") {
			t.Errorf("%q:\n%s", c.keys, s.text)
		}
	}
}

func TestFormBackKeepsValues(t *testing.T) {
	h, _ := formHost(t)
	code := h.form("192.0.2.10", enterKey, esc, enterKey, key, enterKey, " ", btab, enterKey, enterKey, enterKey,
		down, enterKey, esc, esc, enterKey, up, enterKey, right, enterKey, enterKey, left, left, left, enterKey)
	through(t, "back", code, h)
	// the last address screen is the one after Esc on the key screen, the last key screen
	// the one after Shift-Tab on the guests
	s := lastOf(t, h, "step 1 of 5")
	if !strings.Contains(s.main(), "> 192.0.2.10") {
		t.Fatalf("address not kept:\n%s", s.text)
	}
	if fg, bg, _ := s.styleOf(t, "192.0.2.10").Decompose(); bg != tcell.NewRGBColor(0, 0, 0xaa) || fg != tcell.NewRGBColor(0xff, 0xff, 0xff) {
		t.Fatalf("address not shown as a value: %v on %v", fg, bg)
	}
	if s := lastOf(t, h, "step 2 of 5"); !strings.Contains(s.text, "BQUFBQUFBQUFBQUFB") {
		t.Fatalf("key not kept:\n%s", s.text)
	}
	if text, _ := h.file(t, confFile); !strings.Contains(text, "VMIDS=100\n") || !strings.Contains(text, "FLEECING_STORAGE=fast-ssd\n") {
		t.Fatal(text)
	}
	// Cancel on the summary: nothing changed
	h, _ = formHost(t)
	h.stopped(t, "cancel button", h.form("192.0.2.10", enterKey, key, enterKey, " ", enterKey, enterKey, enterKey, tab, tab, enterKey), "cancelled")
	// the keys run out: the terminal ended
	h, term := formHost(t)
	h.stopped(t, "eof", h.form("192.0.2.10", enterKey), "the terminal closed")
	if term.stopped != 1 {
		t.Fatal("terminal not restored")
	}
}

func TestFormReinstallPrefill(t *testing.T) {
	h, _ := formHost(t)
	h.sys.Plain = true
	if code := h.install(t, answers("192.0.2.10", key, "101 100", "local", "", "y")); code != 0 {
		t.Fatal(h.errb.String())
	}
	h.sys.Plain = false
	for _, name := range []string{"usr/bin/sudo", "usr/sbin/visudo"} {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(h.root, name)), 0o755)
		_ = os.WriteFile(filepath.Join(h.root, name), nil, 0o755)
	}
	through(t, "reinstall", h.form(enterKey, enterKey, enterKey, enterKey, enterKey, enterKey), h)
	out := h.out.String()
	if strings.Count(out, "unchanged ") != 6 || strings.Contains(out, "written ") {
		t.Fatalf("not idempotent:\n%s", out)
	}
	s := screenOf(t, h, "step 1 of 5")
	if _, bg, _ := s.styleOf(t, "192.0.2.10").Decompose(); bg != tcell.NewRGBColor(0, 0, 0xaa) {
		t.Fatalf("address not a real value:\n%s", s.text)
	}
	s = screenOf(t, h, "step 3 of 5")
	if !strings.Contains(s.text, "[x] 100") || !strings.Contains(s.text, "[x] 101") || !strings.Contains(s.text, "[ ] 102") {
		t.Fatalf("guests not ticked:\n%s", s.text)
	}
	s = screenOf(t, h, "step 4 of 5")
	if !strings.Contains(s.text, "► (*) local") {
		t.Fatalf("storage not chosen:\n%s", s.text)
	}
}

// NO_COLOR: the same layout, no colours; focus and bars in inverse video.
func TestFormNoColor(t *testing.T) {
	h, _ := formHost(t, "NO_COLOR", "1")
	h.stopped(t, "no color", h.form(enterKey, "192.0.2.10", enterKey, key, enterKey, " ", ctrlC), "cancelled")
	for _, s := range h.shots() {
		for i, c := range s.cells {
			if fg, bg, _ := c.Style.Decompose(); fg != tcell.ColorDefault || bg != tcell.ColorDefault {
				t.Fatalf("colour %v on %v at %d,%d:\n%s", fg, bg, i%s.cols, i/s.cols, s.text)
			}
		}
	}
	s := screenOf(t, h, "step 1 of 5")
	if !strings.HasPrefix(s.line(1), " ┌──") || !strings.Contains(s.line(1), "─ Backup machine address ─") || !strings.Contains(s.help(), "Example: 192.0.2.10") {
		t.Fatalf("layout:\n%s", s.text)
	}
	if _, _, a := s.styleOf(t, " restic-pve install").Decompose(); a&tcell.AttrReverse == 0 {
		t.Error("title bar not inverse")
	}
	s = lastOf(t, h, "step 3 of 5")
	if _, _, a := s.styleOf(t, "► [x] 100").Decompose(); a&tcell.AttrReverse == 0 {
		t.Error("focused row not inverse")
	}
}

// A terminal that is not UTF-8 gets ASCII frames and marks.
func TestFormASCII(t *testing.T) {
	h, term := formHost(t)
	term.charset = "US-ASCII"
	h.stopped(t, "ascii", h.form("192.0.2.10", enterKey, key, enterKey, ctrlC), "cancelled")
	s := screenOf(t, h, "step 3 of 5")
	if !strings.HasPrefix(s.line(1), " +---") || !strings.Contains(s.line(1), "- Guests -") || !strings.Contains(s.text, "> [ ] 100") || !strings.Contains(s.line(23), "| restic-pve") {
		t.Fatalf("not ASCII:\n%s", s.text)
	}
	for _, s := range h.shots() {
		for _, r := range s.text {
			if r > 0x7e {
				t.Fatalf("non-ASCII %q:\n%s", r, s.text)
			}
		}
	}
}

// A long key in a narrow terminal shows its end, and the line fits.
func TestFormNarrowKey(t *testing.T) {
	h, term := formHost(t)
	term.cols = 60
	h.stopped(t, "narrow", h.form("192.0.2.10", enterKey, key+" restic-backup@backup.example", ctrlC), "cancelled")
	s := last(h)
	if !strings.Contains(s.text, "> …") || !strings.Contains(s.text, "restic-backup@backup.example ") {
		t.Fatalf("tail not shown:\n%s", s.text)
	}
	for _, l := range strings.Split(s.text, "\n") {
		if n := len([]rune(l)); n != 60 {
			t.Errorf("line of %d characters: %q", n, l)
		}
	}
}

// At the smallest size the summary buttons and all four limit fields still show.
func TestFormSmallestTerminal(t *testing.T) {
	h, term := formHost(t)
	term.cols, term.rows = minCols, minRows
	keys := []string{"192.0.2.10", enterKey, key, enterKey, " "}
	for i := 0; i < 2; i++ {
		keys = append(keys, "j", " ")
	}
	keys = append(keys, enterKey, enterKey, down, enterKey, enterKey, enterKey, enterKey, enterKey, ctrlC)
	h.stopped(t, "smallest", h.form(keys...), "cancelled")
	s := lastOf(t, h, "Up/Down field")
	for _, w := range []string{"Free space to start", "Free space to stop", "Read limit (MiB/s)", "► Backups at the same time"} {
		if !strings.Contains(s.text, w) {
			t.Errorf("fields lack %q:\n%s", w, s.text)
		}
	}
	if !strings.Contains(s.help(), "How many guests are backed up at once. 1-16.") {
		t.Errorf("help: %q", s.help())
	}
	s = last(h)
	if !strings.Contains(s.text, "[ Install ]  [ Back ]  [ Cancel ]") || !strings.Contains(s.text, "Backup machine") {
		t.Fatalf("summary cut:\n%s", s.text)
	}
	for _, s := range h.shots() {
		if s.rows != minRows || s.cols != minCols {
			t.Fatalf("%dx%d:\n%s", s.cols, s.rows, s.text)
		}
		if !strings.Contains(s.line(minRows-1), "Esc back") {
			t.Fatalf("keys cut: %q", s.line(minRows-1))
		}
	}
}

func TestFormMode(t *testing.T) {
	cases := []struct {
		label      string
		plain, tty bool
		term       string
		cols, rows int
		sizeErr    bool
		want       bool
	}{
		{"terminal", false, true, "xterm", 80, 24, false, true},
		{"smallest", false, true, "linux", 60, 20, false, true},
		{"--plain", true, true, "xterm", 80, 24, false, false},
		{"no terminal", false, false, "xterm", 80, 24, false, false},
		{"TERM empty", false, true, "", 80, 24, false, false},
		{"TERM dumb", false, true, "dumb", 80, 24, false, false},
		{"too narrow", false, true, "xterm", 59, 24, false, false},
		{"too low", false, true, "xterm", 80, 19, false, false},
		{"size unknown is 80x24", false, true, "xterm", 0, 0, true, true},
	}
	for _, c := range cases {
		s := &System{Plain: c.plain, Getenv: func(k string) string {
			if k == "TERM" {
				return c.term
			}
			return ""
		}}
		if c.tty {
			term := &fakeTerm{cols: c.cols, rows: c.rows}
			if c.sizeErr {
				term.sizeErr = io.ErrUnexpectedEOF
			}
			s.Term = term
		}
		if got := s.useForm(); got != c.want {
			t.Errorf("%s: %v", c.label, got)
		}
	}
	// --plain on a terminal asks line by line; so do a terminal that cannot be set up and one
	// whose character set tcell does not know
	for _, label := range []string{"plain", "start fails", "unknown charset"} {
		h, term := formHost(t)
		switch label {
		case "plain":
			h.sys.Plain = true
		case "start fails":
			term.failure = io.ErrClosedPipe
		default:
			term.charset = "no-such-charset"
		}
		if code := h.install(t, fresh); code != 0 || !strings.Contains(h.out.String(), "Enter takes the value in [brackets]") ||
			strings.Contains(h.out.String(), "\x1b[") || term.started != 0 {
			t.Fatalf("%s: %d %s", label, code, h.out.String())
		}
	}
}

func TestFormRestoresTheTerminalOnPanic(t *testing.T) {
	h, term := formHost(t)
	term.panicAfterStart = true
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("no panic")
			}
		}()
		h.form("x")
	}()
	if term.started != 1 || term.stopped != 1 {
		t.Fatalf("started %d stopped %d", term.started, term.stopped)
	}
}

func TestKeys(t *testing.T) {
	names := map[keyKind]string{kEnter: "Enter", kEsc: "Esc", kBackTab: "BackTab", kTab: "Tab", kUp: "Up", kDown: "Down",
		kLeft: "Left", kRight: "Right", kBackspace: "BS", kCtrlC: "C-c", kCtrlU: "C-u", kNone: "-"}
	name := func(k keyIn) string {
		if k.kind == kRune {
			return string(k.r)
		}
		return names[k.kind]
	}
	var got []string
	for _, ev := range []*tcell.EventKey{
		tcell.NewEventKey(tcell.KeyRune, 'a', 0), tcell.NewEventKey(tcell.KeyEnter, 0, 0), tcell.NewEventKey(tcell.KeyLF, 0, 0),
		tcell.NewEventKey(tcell.KeyEsc, 0, 0), tcell.NewEventKey(tcell.KeyUp, 0, 0), tcell.NewEventKey(tcell.KeyDown, 0, 0),
		tcell.NewEventKey(tcell.KeyRight, 0, 0), tcell.NewEventKey(tcell.KeyLeft, 0, 0), tcell.NewEventKey(tcell.KeyBacktab, 0, 0),
		tcell.NewEventKey(tcell.KeyTab, 0, 0), tcell.NewEventKey(tcell.KeyBackspace, 0, 0), tcell.NewEventKey(tcell.KeyBackspace2, 0, 0),
		tcell.NewEventKey(tcell.KeyCtrlC, 0, 0), tcell.NewEventKey(tcell.KeyCtrlU, 0, 0), tcell.NewEventKey(tcell.KeyCtrlA, 0, 0),
		tcell.NewEventKey(tcell.KeyDelete, 0, 0), tcell.NewEventKey(tcell.KeyRune, 'ä', 0),
	} {
		got = append(got, name(keyOf(ev)))
	}
	if s := strings.Join(got, " "); s != "a Enter Enter Esc Up Down Right Left BackTab Tab BS BS C-c C-u - - ä" {
		t.Errorf("keys %q", s)
	}
	for _, c := range []struct{ in, want string }{
		{"ab\r\ncd", "a b Enter"}, {"abcd", "a b c d"}, {"x\ty\n z", "x y Enter"}, {"\n", "Enter"},
	} {
		got = nil
		for _, k := range pasteKeys(c.in) {
			got = append(got, name(k))
		}
		if s := strings.Join(got, " "); s != c.want {
			t.Errorf("paste %q: %q", c.in, s)
		}
	}
}
