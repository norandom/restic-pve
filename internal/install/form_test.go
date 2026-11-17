package install

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// fakeTerm is the terminal of the form tests: a size and counters for Start and stop.
type fakeTerm struct {
	cols, rows       int
	sizeErr, failure error
	started, stopped int
	panicAfterStart  bool
	out              io.Writer // stop writes the end of the alternate screen here
}

func (t *fakeTerm) Size() (int, int, error) {
	if t.panicAfterStart && t.started > 0 {
		panic("size")
	}
	return t.cols, t.rows, t.sizeErr
}

func (t *fakeTerm) Start() (func(), error) {
	if t.failure != nil {
		return nil, t.failure
	}
	t.started++
	return func() {
		t.stopped++
		if t.out != nil {
			io.WriteString(t.out, "\x1b[?1049l")
		}
	}, nil
}

// chunks returns one string per Read, like a terminal returns one key or one paste.
type chunks []string

func (c *chunks) Read(p []byte) (int, error) {
	if len(*c) == 0 {
		return 0, io.EOF
	}
	n := copy(p, (*c)[0])
	if (*c)[0] = (*c)[0][n:]; (*c)[0] == "" {
		*c = (*c)[1:]
	}
	return n, nil
}

// The keys as a terminal in raw mode sends them.
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

// formHost is a fake host whose stdin and stdout are an 80x24 terminal.
func formHost(t *testing.T, env ...string) (*fakeHost, *fakeTerm) {
	h := newHost(t)
	term := &fakeTerm{cols: 80, rows: 24, out: &h.out}
	vars := map[string]string{"TERM": "xterm-256color"}
	for i := 0; i+1 < len(env); i += 2 {
		vars[env[i]] = env[i+1]
	}
	h.sys.Term, h.sys.Getenv = term, func(k string) string { return vars[k] }
	return h, term
}

// form runs install with the keys, each key one read.
func (h *fakeHost) form(keys ...string) int {
	h.out.Reset()
	h.errb.Reset()
	h.cmds = nil
	c := chunks(keys)
	h.sys.Stdin = bufio.NewReader(&c)
	return h.sys.Install()
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z~]`)

// strip removes the escape sequences and carriage returns: the text the admin sees.
func strip(s string) string { return strings.ReplaceAll(ansiRe.ReplaceAllString(s, ""), "\r", "") }

// frames splits the output into the raw screens the form drew (each starts at home).
// The progress lines after the alternate screen are not part of a screen.
func frames(out string) []string {
	if i := strings.LastIndex(out, "\x1b[?1049l"); i >= 0 {
		out = out[:i]
	}
	parts := strings.Split(out, "\x1b[H")
	return parts[1:]
}

// screenOf returns the first visible screen that contains want and its raw text.
func screenOf(t *testing.T, out, want string) (string, string) {
	t.Helper()
	for _, f := range frames(out) {
		if s := strip(f); strings.Contains(s, want) {
			return s, f
		}
	}
	t.Fatalf("no screen with %q in\n%s", want, strip(out))
	return "", ""
}

// lastOf returns the last screen that contains want.
func lastOf(t *testing.T, out, want string) (string, string) {
	t.Helper()
	fs := frames(out)
	for i := len(fs) - 1; i >= 0; i-- {
		if s := strip(fs[i]); strings.Contains(s, want) {
			return s, fs[i]
		}
	}
	t.Fatalf("no screen with %q", want)
	return "", ""
}

// last returns the last screen before the form closed.
func last(out string) string {
	fs := frames(out)
	return strip(fs[len(fs)-1])
}

func through(t *testing.T, label string, code int, h *fakeHost) {
	t.Helper()
	if code != 0 {
		t.Fatalf("%s: %d %s\n%s", label, code, h.errb.String(), strip(h.out.String()))
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
	tail := out[strings.LastIndex(out, "\x1b[H"):]
	if !strings.Contains(tail, "written   /etc/restic-pve.conf\n") || !strings.HasSuffix(out, "ready: user restic-backup, key accepted from 192.0.2.10, guests: 100 101\n"+
		"This host updates restic-pve daily to the latest signed release (restic-pve-update.timer).\n") {
		t.Fatalf("progress lines missing after the form:\n%s", strip(tail))
	}
	if strings.Contains(out, "Enter takes the value in [brackets]") {
		t.Fatal("plain questions shown as well")
	}
}

// The visible texts of every screen, once.
func TestFormScreens(t *testing.T) {
	h, _ := formHost(t)
	through(t, "screens", h.form("192.0.2.10", enterKey, key, enterKey, " ", enterKey, down, enterKey, enterKey, enterKey), h)
	out := h.out.String()
	cases := []struct {
		title string
		want  []string
	}{
		{"restic-pve install · step 1 of 5: Backup machine address", []string{
			"Type the IP address of the backup machine", "Only this address may log in with the backup key.",
			"Run ./setup.sh --proxmox on the backup machine; it prints this address.",
			"\n  > 192.0.2.10\n", "Enter continue · Esc back · Ctrl-U clear · Ctrl-C cancel"}},
		{"restic-pve install · step 2 of 5: Public key", []string{
			"Paste the whole public key line of the backup machine. It starts with ssh-ed25519",
			"A public key is not a secret.", "\n  > ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI...\n"}},
		{"restic-pve install · step 3 of 5: Guests", []string{
			"Tick the virtual machines to back up.", "NUMBER     NAME     STATUS",
			"\n  > [ ] 100        web      running\n", "\n    [ ] 102        scratch  stopped\n", "0 ticked",
			"Up/Down move · Space tick · Enter continue · Esc back · Ctrl-C cancel"}},
		{"restic-pve install · step 4 of 5: Fleecing storage", []string{
			"temporary data while a running guest is read, so the guest", "most free space on fast disks",
			"NAME      TYPE        FREE GiB", "\n  > (*) fast-ssd  lvmthin          819\n", "\n    ( ) local     dir               77\n",
			"Up/Down move · Enter choose · Esc back · Ctrl-C cancel"}},
		{"restic-pve install · step 5 of 5: Limits", []string{
			"These limits protect this host while a backup runs.",
			"Start a backup only with       32 GiB", "Stop a running backup below    8 GiB",
			"Read limit                     40 MiB/s", "Backups at the same time       1",
			"\n  > Keep these (recommended)\n", "\n    Change\n"}},
		{"restic-pve install · summary", []string{
			"a backup user with its key, a sudoers file, the binary and a daily update timer. Nothing changes before you choose Install.", "Backup machine      192.0.2.10",
			"Public key          ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEFB", "Guests              100 (web)",
			"Fleecing storage    local", "Limits              start with 32 GiB free\n                      stop below 8 GiB free\n                      read 40 MiB/s, 1 at a time\n",
			"Backup user         restic-backup", "[ Install ]  [ Back ]  [ Cancel ]",
			"Left/Right move · Enter press · Esc back · Ctrl-C cancel"}},
	}
	for _, c := range cases {
		s, _ := screenOf(t, out, c.title)
		if n := strings.Count(s, "\n"); n != 23 {
			t.Errorf("%s: %d lines, want 24", c.title, n+1)
		}
		flat := strings.Join(strings.Fields(s), " ") // prose may wrap anywhere
		for _, w := range c.want {
			if exact := strings.Contains(w, "\n") || strings.Contains(w, "  "); exact && !strings.Contains(s, w) || !exact && !strings.Contains(flat, w) {
				t.Errorf("%s lacks %q:\n%s", c.title, w, s)
			}
		}
	}
	_, raw := screenOf(t, out, "step 1 of 5")
	if !strings.Contains(raw, "  > \x1b[2m192.0.2.10\x1b[0m") {
		t.Errorf("example not grey: %q", raw)
	}
	_, raw = screenOf(t, out, "summary")
	if !strings.Contains(raw, "\x1b[7m[ Install ]\x1b[0m") {
		t.Errorf("Install not focused: %q", raw)
	}
	_, raw = screenOf(t, out, "step 5 of 5: Limits")
	if !strings.Contains(raw, "\x1b[?25l") || strings.Contains(raw, "\x1b[?25h") {
		t.Errorf("cursor shown on a list: %q", raw)
	}
}

func TestFormExampleIsNeverTheValue(t *testing.T) {
	h, term := formHost(t)
	code := h.form(enterKey, "192.0.2.10", enterKey, enterKey, ctrlC)
	h.stopped(t, "examples", code, "cancelled")
	out := h.out.String()
	s, _ := screenOf(t, out, "an address or pattern for authorized_keys from=")
	if !strings.Contains(s, "step 1 of 5") {
		t.Fatalf("left the address screen:\n%s", s)
	}
	s, _ = screenOf(t, out, "one line 'ssh-ed25519 <base64> [comment]'")
	if !strings.Contains(s, "step 2 of 5") {
		t.Fatalf("left the key screen:\n%s", s)
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
		s := last(h.out.String())
		if !strings.Contains(s, c.rule) || !strings.Contains(s, c.step) {
			t.Errorf("%q: rule or step missing:\n%s", c.keys, s)
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
	s, _ := screenOf(t, h.out.String(), "> [x] 115")
	if !strings.Contains(s, "more above)") || strings.Contains(s, "guest-00") || strings.Count(s, "\n") != 19 {
		t.Fatalf("list did not scroll:\n%s", s)
	}
	s, _ = screenOf(t, h.out.String(), "step 3 of 5")
	if !strings.Contains(s, "more below)") || !strings.Contains(s, "guest-00") {
		t.Fatalf("first guest screen:\n%s", s)
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
	out := h.out.String()
	s, _ := screenOf(t, out, "FLEECING_ABORT_FREE_GIB must be below FLEECING_MIN_FREE_GIB (64)")
	if !strings.Contains(s, "  > Free space to stop (GiB)   64") {
		t.Fatalf("not on the stop field:\n%s", s)
	}
	s, _ = screenOf(t, out, "Up/Down field")
	for _, w := range []string{"  > Free space to start (GiB)  32", "    Free space to stop (GiB)   8", "    Read limit (MiB/s)         40",
		"    Backups at the same time   1", "A backup starts only when the fleecing storage has this much free space.\n      1-999999.",
		"start value, 1-999999.", "1-976562.", "How many guests are backed up at once. 1-16."} {
		if !strings.Contains(s, w) {
			t.Errorf("fields lack %q:\n%s", w, s)
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
		s := last(h.out.String())
		if !strings.Contains(s, "must be") || !strings.Contains(s, "Up/Down field") {
			t.Errorf("%q:\n%s", c.keys, s)
		}
	}
}

func TestFormBackKeepsValues(t *testing.T) {
	h, _ := formHost(t)
	code := h.form("192.0.2.10", enterKey, esc, enterKey, key, enterKey, " ", btab, enterKey, enterKey, enterKey,
		down, enterKey, esc, esc, enterKey, up, enterKey, right, enterKey, enterKey, left, left, left, enterKey)
	through(t, "back", code, h)
	out := h.out.String()
	// the last address screen is the one after Esc on the key screen, the last key screen
	// the one after Shift-Tab on the guests
	if s, raw := lastOf(t, out, "step 1 of 5"); !strings.Contains(s, "  > 192.0.2.10\n") || strings.Contains(raw, "\x1b[2m192.0.2.10") {
		t.Fatalf("address not kept as a value:\n%s", s)
	}
	if s, _ := lastOf(t, out, "step 2 of 5"); !strings.Contains(s, "BQUFBQUFBQUFBQUFB\n") {
		t.Fatalf("key not kept:\n%s", s)
	}
	if text, _ := h.file(t, confFile); !strings.Contains(text, "VMIDS=100\n") || !strings.Contains(text, "FLEECING_STORAGE=fast-ssd\n") {
		t.Fatal(text)
	}
	// Cancel on the summary: nothing changed
	h, _ = formHost(t)
	h.stopped(t, "cancel button", h.form("192.0.2.10", enterKey, key, enterKey, " ", enterKey, enterKey, enterKey, tab, tab, enterKey), "cancelled")
	// the keys run out
	h, _ = formHost(t)
	h.stopped(t, "eof", h.form("192.0.2.10", enterKey), "the terminal closed")
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
		t.Fatalf("not idempotent:\n%s", strip(out))
	}
	_, raw := screenOf(t, out, "step 1 of 5")
	if !strings.Contains(raw, "  > 192.0.2.10\x1b[K") {
		t.Fatalf("address not a real value: %q", raw)
	}
	s, _ := screenOf(t, out, "step 3 of 5")
	if !strings.Contains(s, "[x] 100") || !strings.Contains(s, "[x] 101") || !strings.Contains(s, "[ ] 102") {
		t.Fatalf("guests not ticked:\n%s", s)
	}
	s, _ = screenOf(t, out, "step 4 of 5")
	if !strings.Contains(s, "  > (*) local") {
		t.Fatalf("storage not chosen:\n%s", s)
	}
}

func TestFormNoColor(t *testing.T) {
	h, _ := formHost(t, "NO_COLOR", "1")
	h.stopped(t, "no color", h.form(enterKey, "192.0.2.10", enterKey, ctrlC), "cancelled")
	out := h.out.String()
	for _, c := range []string{"\x1b[2m", "\x1b[31m"} {
		if strings.Contains(out, c) {
			t.Errorf("colour %q with NO_COLOR", c)
		}
	}
	s, _ := screenOf(t, out, "step 1 of 5")
	if !strings.Contains(s, "  > \n\n    e.g. 192.0.2.10\n") {
		t.Fatalf("no example line:\n%s", s)
	}
	s, _ = screenOf(t, out, "step 2 of 5")
	if !strings.Contains(s, "    e.g. ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI...\n") {
		t.Fatalf("no example line:\n%s", s)
	}
}

// A long key in a narrow terminal shows its end, and the line fits.
func TestFormNarrowKey(t *testing.T) {
	h, term := formHost(t)
	term.cols = 60
	h.stopped(t, "narrow", h.form("192.0.2.10", enterKey, key+" restic-backup@backup.example", ctrlC), "cancelled")
	s := last(h.out.String())
	if !strings.Contains(s, "  > …") || !strings.Contains(s, "restic-backup@backup.example\n") {
		t.Fatalf("tail not shown:\n%s", s)
	}
	for _, l := range strings.Split(s, "\n") {
		if n := len([]rune(l)); n > 59 {
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
	out := h.out.String()
	s, _ := lastOf(t, out, "Up/Down field")
	for _, w := range []string{"Free space to start", "Free space to stop", "Read limit (MiB/s)", "  > Backups at the same time", "How many guests are backed up at once. 1-16."} {
		if !strings.Contains(s, w) {
			t.Errorf("fields lack %q:\n%s", w, s)
		}
	}
	s = last(out)
	if !strings.Contains(s, "[ Install ]  [ Back ]  [ Cancel ]") || !strings.Contains(s, "Backup user") {
		t.Fatalf("summary cut:\n%s", s)
	}
	for _, f := range frames(out) {
		if v := strip(f); strings.Count(v, "\n") != minRows-1 {
			t.Fatalf("%d lines:\n%s", strings.Count(v, "\n")+1, v)
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
	// --plain on a terminal asks line by line; so does a terminal that cannot be set up
	for _, label := range []string{"plain", "start fails"} {
		h, term := formHost(t)
		if label == "plain" {
			h.sys.Plain = true
		} else {
			term.failure = io.ErrClosedPipe
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

func TestKeyReader(t *testing.T) {
	names := map[keyKind]string{kEnter: "Enter", kEsc: "Esc", kBackTab: "BackTab", kTab: "Tab", kUp: "Up", kDown: "Down",
		kLeft: "Left", kRight: "Right", kBackspace: "BS", kCtrlC: "C-c", kCtrlU: "C-u"}
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"a\r", "\n", "b\n", "\r\n"}, "a Enter b Enter Enter"},
		{[]string{"\x1b", "\x1b[A\x1bOB\x1b[C\x1b[D\x1b[Z\t"}, "Esc Up Down Right Left BackTab Tab"},
		{[]string{"\x1b[3~x\x1bq"}, "x Esc q"},
		{[]string{"\x1b[", "B"}, "Down"},
		{[]string{pasteA + "ab\r\ncd" + pasteZ + "e"}, "a b Enter e"},
		{[]string{pasteA + "ab", "cd" + pasteZ}, "a b c d"},
		{[]string{"\x7f\x08\x03\x15\x01"}, "BS BS C-c C-u"},
		{[]string{"\xc3", "\xa4"}, "ä"},
	}
	for _, c := range cases {
		ch := chunks(c.in)
		k := &keyReader{r: &ch}
		var got []string
		for {
			kk := k.next()
			if kk.kind == kEOF {
				break
			}
			if kk.kind == kRune {
				got = append(got, string(kk.r))
			} else {
				got = append(got, names[kk.kind])
			}
		}
		if strings.Join(got, " ") != c.want {
			t.Errorf("%q: %q", c.in, strings.Join(got, " "))
		}
	}
}
