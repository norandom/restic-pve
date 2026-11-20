package install

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/norandom/restic-pve/internal/provider"
	"github.com/norandom/restic-pve/internal/wrapper"
)

// key is a valid OpenSSH ed25519 public key (32 bytes 'A').
var key = "ssh-ed25519 " + base64.StdEncoding.EncodeToString(append(append([]byte{}, ed25519Blob...), bytes.Repeat([]byte("A"), 32)...))

const qmList = `      VMID NAME                 STATUS     MEM(MB)    BOOTDISK(GB) PID
       100 web                  running    2048              32.00 4242
       101 db                   stopped    1024              16.00 0
       102 scratch              stopped    512                8.00 0
`

const pvesmStatus = `Name             Type     Status           Total            Used       Available        %
fast-ssd      lvmthin     active       959786032       100000000       859786032   10.42%
local             dir     active        98497780        17497780        81000000   17.76%
nfs-old           nfs   inactive               0               0               0    0.00%
restic-pve  resticpve     active               0               0               0    0.00%
`

// answers joins one answer per line.
func answers(a ...string) string { return strings.Join(a, "\n") + "\n" }

// fresh are the typed answers of a first install; enter takes every default and confirms.
var (
	fresh = answers("192.0.2.10", key+" restic-backup@backup.example", "100, 101", "fast-ssd", "", "", "y")
	enter = answers("", "", "", "", "", "", "y")
)

// fakeHost simulates the account database, qm and pvesm, and records commands and chowns.
type fakeHost struct {
	root          string
	user          string
	qm            string // the output of qm list
	cmds          []string
	owners        map[string]string
	exists        bool
	shell         string
	locked        bool
	visudoFails   bool
	out, errb     bytes.Buffer
	sys           *System
	exe, exeBytes string
	apiOut        string // what the Proxmox API check prints
	loadOut       string // what the load check prints
	failCmd       string // a command prefix that fails
}

func newHost(t *testing.T) *fakeHost {
	root := t.TempDir()
	for _, d := range []string{"etc", "usr/local/sbin", "usr/sbin", "home", "run"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, tool := range []string{"qm", "pvesm"} {
		if err := os.WriteFile(filepath.Join(root, "usr/sbin", tool), nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h := &fakeHost{root: root, user: DefaultUser, qm: qmList, owners: map[string]string{}, exe: filepath.Join(t.TempDir(), "restic-pve"), exeBytes: "binary v1\n",
		apiOut: "api 17 8 base 1\n", loadOut: "loaded\n"}
	if err := os.WriteFile(h.exe, []byte(h.exeBytes), 0o755); err != nil {
		t.Fatal(err)
	}
	h.sys = &System{Root: root, Run: h.run, Executable: h.exe, IsRoot: func() bool { return true },
		Hostname: func() (string, error) { return "pve1.example.org", nil },
		Chown: func(p string, uid, gid int) error {
			rel, _ := filepath.Rel(root, p)
			if base := filepath.Base(rel); strings.HasPrefix(base, ".") && strings.Contains(base, ".new-") {
				rel = filepath.Join(filepath.Dir(rel), base[1:strings.Index(base, ".new-")]) // temp file of putFile
			}
			h.owners["/"+rel] = strconv.Itoa(uid) + ":" + strconv.Itoa(gid)
			return nil
		}, Version: "v1.2.3", Stdout: &h.out, Stderr: &h.errb}
	return h
}

func (h *fakeHost) run(name string, args ...string) ([]byte, error) {
	cmd := strings.Join(append([]string{name}, args...), " ")
	if name == "timeout" && len(args) == 4 && args[1] == "perl" {
		cmd = map[bool]string{true: "perl load", false: "perl api"}[strings.Contains(args[3], "lookup")]
	}
	h.cmds = append(h.cmds, cmd)
	u := h.user
	switch {
	case cmd == "qm list":
		return []byte(h.qm), nil
	case cmd == "pvesm status":
		return []byte(pvesmStatus), nil
	case cmd == "getent passwd "+u:
		if !h.exists {
			return nil, errors.New("exit status 2")
		}
		return []byte(u + ":x:999:998:restic backup:/home/" + u + ":" + h.shell + "\n"), nil
	case cmd == "getent shadow "+u:
		if h.locked {
			return []byte(u + ":!:20000::::::\n"), nil
		}
		return []byte(u + ":*:20000::::::\n"), nil
	case strings.HasPrefix(cmd, "useradd "):
		h.exists, h.shell = true, "/bin/sh"
	case cmd == "usermod --shell /bin/sh "+u:
		h.shell = "/bin/sh"
	case cmd == "usermod --lock "+u:
		h.locked = true
	case cmd == "userdel --remove "+u:
		h.exists = false
	case name == "visudo":
		data, err := os.ReadFile(args[1])
		if err != nil || h.visudoFails || !strings.Contains(string(data), "\n"+sudoersLine(u)+"\n") {
			return nil, errors.New("exit status 1")
		}
	case strings.HasPrefix(cmd, "getent "):
		return nil, errors.New("exit status 2")
	case name == "timeout" && len(args) == 4 && args[1] == "perl": // the Proxmox checks
		if h.failCmd != "" && strings.HasPrefix("perl", h.failCmd) {
			return nil, errors.New("exit status 124")
		}
		if strings.Contains(args[3], "lookup") {
			return []byte(h.loadOut), nil
		}
		return []byte(h.apiOut), nil
	case h.failCmd != "" && strings.HasPrefix(cmd, h.failCmd):
		return nil, errors.New("exit status 1")
	case strings.HasPrefix(cmd, "pvesm add resticpve restic-pve "):
		cfg := filepath.Join(h.root, provider.StorageConfig)
		old, _ := os.ReadFile(cfg)
		_ = os.MkdirAll(filepath.Dir(cfg), 0o755)
		_ = os.WriteFile(cfg, append(old, "resticpve: restic-pve\n\tcontent backup\n\tnodes "+args[6]+"\n"...), 0o640)
	case cmd == "pvesm remove restic-pve":
		cfg := filepath.Join(h.root, provider.StorageConfig)
		old, _ := os.ReadFile(cfg)
		_ = os.WriteFile(cfg, []byte(strings.Replace(string(old), "resticpve: restic-pve\n\tcontent backup\n\tnodes pve1\n", "", 1)), 0o640)
	}
	return nil, nil
}

func (h *fakeHost) install(t *testing.T, input string) int {
	h.out.Reset()
	h.errb.Reset()
	h.cmds = nil
	h.sys.Stdin = bufio.NewReader(strings.NewReader(input))
	return h.sys.Install()
}

func (h *fakeHost) file(t *testing.T, p string) (string, os.FileMode) {
	t.Helper()
	full := filepath.Join(h.root, p)
	data, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(full)
	return string(data), st.Mode().Perm()
}

// stopped checks a run that ended before the install: exit 1, one stderr line ending in
// "nothing changed", no file written and no command that changes the host.
func (h *fakeHost) stopped(t *testing.T, label string, code int, want string) {
	t.Helper()
	if code != 1 || h.errb.String() != "restic-pve: "+want+"; nothing changed\n" {
		t.Errorf("%s: %d %q", label, code, h.errb.String())
	}
	for _, c := range h.cmds {
		if c != "qm list" && c != "pvesm status" && !strings.HasPrefix(c, "getent ") {
			t.Errorf("%s: ran %q", label, c)
		}
	}
	if ents, _ := os.ReadDir(filepath.Join(h.root, "etc")); len(ents) != 0 {
		t.Errorf("%s: wrote %v", label, ents)
	}
	if strings.Contains(h.out.String(), "written") {
		t.Errorf("%s: %s", label, h.out.String())
	}
}

const confText = generated + "\nFLEECING_STORAGE=fast-ssd\nFLEECING_MIN_FREE_GIB=32\nFLEECING_ABORT_FREE_GIB=8\nBWLIMIT_KIB=40960\nMAX_WORKERS=1\nVMIDS=100 101\n"

func TestInstallFreshHostThenEnterOnly(t *testing.T) {
	h := newHost(t)
	if code := h.install(t, fresh); code != 0 {
		t.Fatalf("%d %s", code, h.errb.String())
	}
	t.Log("\n" + h.out.String())
	if h.cmds[0] != "qm list" || h.cmds[1] != "pvesm status" || !strings.Contains(strings.Join(h.cmds, "\n"),
		"\napt-get update -qq\napt-get install -y -qq sudo\n") {
		t.Fatalf("lists not read first or sudo missing but not installed: %q", h.cmds)
	}
	if !strings.Contains(strings.Join(h.cmds, "\n"), "\nuseradd --system --create-home --shell /bin/sh --comment restic backup (forced command only) restic-backup\n") {
		t.Fatalf("%q", h.cmds)
	}
	out := h.out.String()
	for _, want := range []string{
		"Backup machine address the key is accepted from, e.g. 192.0.2.10 (authorized_keys from=): ",
		"the whole line, e.g. ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI...\n",
		"(./setup.sh --proxmox on the backup machine shows it): ",
		"  100   web      running\n", "  102   scratch  stopped\n",
		"VM numbers to back up, separated by spaces or commas: ",
		"  fast-ssd  lvmthin  819\n", "  local     dir      77\n",
		"Fleecing storage: ",
		"Limits: FLEECING_MIN_FREE_GIB 32, FLEECING_ABORT_FREE_GIB 8, read limit 40 MiB/s (BWLIMIT_KIB 40960), MAX_WORKERS 1\nChange the limits? [y/N]: ",
		"  authorized_keys   from=\"192.0.2.10\", key " + key + "\n",
		"guests 100 101, fleecing storage fast-ssd,",
		"Install with these values? [y/N]: ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dialogue lacks %q", want)
		}
	}
	if strings.Contains(out, "nfs-old") || strings.Contains(out, "  restic-pve  ") {
		t.Error("inactive storage or restic-pve's own storage listed for fleecing")
	}
	files := []struct {
		path, text string
		mode       os.FileMode
		owner      string
	}{
		{"/home/restic-backup/.ssh/authorized_keys", `restrict,from="192.0.2.10",command="sudo -n /usr/local/sbin/restic-pve request $SSH_ORIGINAL_COMMAND" ` + key + "\n", 0o600, "999:998"},
		{"/etc/sudoers.d/restic-pve", generated + "\nrestic-backup ALL=(root) NOPASSWD: /usr/local/sbin/restic-pve request, /usr/local/sbin/restic-pve request *\n", 0o440, "0:0"},
		{"/usr/local/sbin/restic-pve", h.exeBytes, 0o755, "0:0"},
		{"/etc/restic-pve.conf", confText, 0o644, "0:0"},
		{"/etc/systemd/system/restic-pve-update.service", string(serviceUnit), 0o644, "0:0"},
		{"/etc/systemd/system/restic-pve-update.timer", string(timerUnit), 0o644, "0:0"},
	}
	for _, f := range files {
		text, mode := h.file(t, f.path)
		if text != f.text || mode != f.mode {
			t.Errorf("%s: %o %q", f.path, mode, text)
		}
		if h.owners[f.path] != f.owner {
			t.Errorf("%s owner %q", f.path, h.owners[f.path])
		}
		if !strings.Contains(out, "written   "+f.path+"\n") {
			t.Errorf("not reported: %s", f.path)
		}
	}
	for p, want := range map[string]os.FileMode{"/home/restic-backup": 0o750, "/home/restic-backup/.ssh": 0o700} {
		if st, err := os.Stat(filepath.Join(h.root, p)); err != nil || st.Mode().Perm() != want || h.owners[p] != "999:998" {
			t.Errorf("%s: %v %v %s", p, st.Mode(), err, h.owners[p])
		}
	}
	if !strings.HasSuffix(out, "restic-pve v1.2.3 ready: user restic-backup, key accepted from 192.0.2.10, guests: 100 101\n"+
		"This host updates restic-pve daily to the latest signed release (restic-pve-update.timer).\n") {
		t.Fatalf("%s", out)
	}
	if !strings.Contains(out, "  update timer      restic-pve-update.timer: restic-pve update daily (always on)\n") {
		t.Fatalf("summary lacks the timer: %s", out)
	}
	timer := "\nsystemctl daemon-reload\nsystemctl enable --now restic-pve-update.timer"
	if cmds := strings.Join(h.cmds, "\n"); !strings.HasSuffix(cmds, timer) {
		t.Fatalf("systemctl calls missing or out of order: %q", h.cmds)
	}

	for _, name := range []string{"usr/bin/sudo", "usr/sbin/visudo"} {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(h.root, name)), 0o755)
		_ = os.WriteFile(filepath.Join(h.root, name), nil, 0o755)
	}
	if code := h.install(t, enter); code != 0 {
		t.Fatalf("second run %d %s", code, h.errb.String())
	}
	out = h.out.String()
	if strings.Contains(out, "written   ") || strings.Contains(out, "created   ") || strings.Contains(out, "locked    ") || strings.Contains(out, "installing") || strings.Count(out, "unchanged ") != 6 {
		t.Fatalf("Enter only is not idempotent:\n%s", out)
	}
	for _, want := range []string{"(authorized_keys from=) [192.0.2.10]: ", "shows it) [" + key + "]: ",
		"commas [100 101]: ", "Fleecing storage [fast-ssd]: "} {
		if !strings.Contains(out, want) {
			t.Errorf("default not shown: %q", want)
		}
	}
	if strings.Join(h.cmds, "\n") != "qm list\npvesm status\ngetent passwd restic-backup\ngetent passwd restic-backup\ngetent shadow restic-backup"+timer {
		t.Fatalf("second run commands %q", h.cmds)
	}
}

// The embedded units: a oneshot "restic-pve update" and its daily timer, always on.
func TestUpdateUnits(t *testing.T) {
	for text, lines := range map[string][]string{
		string(serviceUnit): {"[Service]", "Type=oneshot", "ExecStart=" + Binary + " update"},
		string(timerUnit): {"[Timer]", "OnCalendar=daily", "RandomizedDelaySec=6h", "Persistent=true",
			"[Install]", "WantedBy=timers.target"},
	} {
		if !strings.HasPrefix(text, generated+"\n") {
			t.Errorf("no generated header:\n%s", text)
		}
		for _, l := range lines {
			if !strings.Contains("\n"+text, "\n"+l+"\n") {
				t.Errorf("lacks %q:\n%s", l, text)
			}
		}
	}
	if units[1].path != "/etc/systemd/system/restic-pve-update.timer" || units[0].path != "/etc/systemd/system/restic-pve-update.service" {
		t.Fatal(units)
	}
}

func TestInstallChangesGuestsAndLimits(t *testing.T) {
	h := newHost(t)
	if code := h.install(t, fresh); code != 0 {
		t.Fatal(h.errb.String())
	}
	if code := h.install(t, answers("", "", "102 100", "local", "y", "64", "16", "20", "2", "", "y")); code != 0 {
		t.Fatal(h.errb.String())
	}
	out := h.out.String()
	if !strings.Contains(out, "unchanged /home/restic-backup/.ssh/authorized_keys\n") || !strings.Contains(out, "written   /etc/restic-pve.conf\n") {
		t.Fatal(out)
	}
	for _, want := range []string{"(FLEECING_MIN_FREE_GIB) [32]: ", "(FLEECING_ABORT_FREE_GIB) [8]: ", "(stored as BWLIMIT_KIB) [40]: ", "(MAX_WORKERS) [1]: "} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	conf, _ := h.file(t, confFile)
	c, err := wrapper.ParseConf(confFile, []byte(conf))
	want := wrapper.Conf{Storage: "local", MinFreeGiB: "64", AbortFreeGiB: "16", BwlimitKiB: "20480", MaxWorkers: "2",
		VMIDs: []string{"102", "100"}}
	if err != nil || !reflect.DeepEqual(c, want) || !c.Allowed("102") || c.Allowed("101") {
		t.Fatalf("%+v %v", c, err)
	}
	// the changed values are the next defaults
	if code := h.install(t, answers("", "", "", "", "y", "", "", "", "", "", "y")); code != 0 || strings.Count(h.out.String(), "unchanged ") != 6 {
		t.Fatalf("%d\n%s", code, h.out.String())
	}
}

func TestInstallLegacyHost(t *testing.T) {
	h := newHost(t)
	h.exists, h.shell = true, "/usr/sbin/nologin"
	old := map[string]string{"/usr/local/sbin/restic-vzdump": "#!/bin/bash\n", "/etc/sudoers.d/restic-vzdump": "x\n",
		"/etc/restic-vzdump.allow": "999\n", "/etc/restic-vzdump.conf": "MAX_WORKERS=1\n",
		"/home/restic-backup/.ssh/authorized_keys": "old\n"}
	for p, text := range old {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(h.root, p)), 0o755)
		_ = os.WriteFile(filepath.Join(h.root, p), []byte(text), 0o644)
	}
	if code := h.install(t, fresh); code != 0 {
		t.Fatal(h.errb.String())
	}
	out := h.out.String()
	for _, want := range []string{"changed   login shell of restic-backup to /bin/sh\n", "locked    password of restic-backup\n",
		"removed   /usr/local/sbin/restic-vzdump\n", "removed   /etc/sudoers.d/restic-vzdump\n",
		"removed   /etc/restic-vzdump.conf\n", "removed   /etc/restic-vzdump.allow\n", "written   /etc/restic-pve.conf\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	for _, p := range oldFiles {
		if _, err := os.Lstat(filepath.Join(h.root, p)); err == nil {
			t.Errorf("%s still there", p)
		}
	}
}

func TestInstallKeepsTheUserOfAnOlderInstall(t *testing.T) {
	h := newHost(t)
	h.user, h.exists, h.shell, h.locked = "bk-user", true, "/bin/sh", true
	_ = os.MkdirAll(filepath.Join(h.root, sudoersDir), 0o755)
	_ = os.WriteFile(filepath.Join(h.root, sudoers), []byte(generated+"\n"+sudoersLine("bk-user")+"\n"), 0o440)
	if code := h.install(t, fresh); code != 0 {
		t.Fatal(h.errb.String())
	}
	if !strings.Contains(h.out.String(), "user              bk-user ") || !strings.Contains(h.out.String(), "ready: user bk-user, key accepted from 192.0.2.10, guests: 100 101\n") {
		t.Fatal(h.out.String())
	}
	if _, err := os.Stat(filepath.Join(h.root, "home/bk-user/.ssh/authorized_keys")); err != nil {
		t.Fatal(err)
	}
}

// The documented install downloads the binary straight to /usr/local/sbin/restic-pve and
// runs it from there: the file is not rewritten or truncated, owner and mode are set.
func TestInstallRunningFromTheDestination(t *testing.T) {
	h := newHost(t)
	dst := filepath.Join(h.root, Binary)
	if err := os.WriteFile(dst, []byte(h.exeBytes), 0o700); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(dst)
	h.sys.Executable = dst
	if code := h.install(t, fresh); code != 0 {
		t.Fatalf("%d %s", code, h.errb.String())
	}
	text, mode := h.file(t, Binary)
	after, _ := os.Stat(dst)
	if text != h.exeBytes || mode != 0o755 || h.owners[Binary] != "0:0" || !os.SameFile(before, after) {
		t.Fatalf("%o %q owner %q same inode %v", mode, text, h.owners[Binary], os.SameFile(before, after))
	}
	if out := h.out.String(); !strings.Contains(out, "unchanged "+Binary+"\n") || strings.Contains(out, "written   "+Binary) {
		t.Fatal(out)
	}
	if ents, _ := os.ReadDir(filepath.Dir(dst)); len(ents) != 1 {
		t.Fatalf("temp file left: %v", ents)
	}
}

func TestInstallVisudoFailureKeepsOldSudoers(t *testing.T) {
	h := newHost(t)
	_ = os.MkdirAll(filepath.Join(h.root, sudoersDir), 0o755)
	_ = os.WriteFile(filepath.Join(h.root, sudoers), []byte("old\n"), 0o440)
	h.visudoFails = true
	if code := h.install(t, fresh); code != 1 || !strings.Contains(h.errb.String(), "visudo -cf rejected") {
		t.Fatalf("%d %s", code, h.errb.String())
	}
	if text, _ := h.file(t, sudoers); text != "old\n" {
		t.Fatalf("sudoers replaced: %q", text)
	}
	if ents, _ := os.ReadDir(filepath.Join(h.root, sudoersDir)); len(ents) != 1 {
		t.Fatalf("temp file left: %v", ents)
	}
}

func TestInstallRefusedHosts(t *testing.T) {
	h := newHost(t)
	h.sys.IsRoot = func() bool { return false }
	if code := h.install(t, fresh); code != 1 || h.errb.String() != "restic-pve: install must run as root on the Proxmox VE host\n" || len(h.cmds) != 0 {
		t.Fatalf("not root: %d %q", code, h.errb.String())
	}
	for _, tool := range []string{"qm", "pvesm"} {
		h := newHost(t)
		_ = os.Remove(filepath.Join(h.root, "usr/sbin", tool))
		code := h.install(t, fresh)
		if code != 1 || h.errb.String() != "restic-pve: "+tool+" not found in "+searchPath+": this is not a Proxmox VE host; nothing changed\n" ||
			len(h.cmds) != 0 || h.out.Len() != 0 {
			t.Errorf("%s missing: %d %q", tool, code, h.errb.String())
		}
	}
}

func TestInstallStops(t *testing.T) {
	cases := map[string]struct{ input, want string }{
		"empty stdin":       {"", "no terminal to ask on"},
		"eof in questions":  {answers("192.0.2.10", key), "no terminal to ask on"},
		"eof at confirm":    {answers("192.0.2.10", key, "100", "fast-ssd", ""), "no terminal to ask on"},
		"no at confirm":     {answers("192.0.2.10", key, "100", "fast-ssd", "", "", "n"), "not confirmed"},
		"enter at confirm":  {answers("192.0.2.10", key, "100", "fast-ssd", "", "", ""), "not confirmed"},
		"other at confirm":  {answers("192.0.2.10", key, "100", "fast-ssd", "", "", "ok"), "not confirmed"},
		"three bad from":    {answers("", "a b", `x"y`), "3 invalid answers"},
		"three unknown VMs": {answers("192.0.2.10", key, "999", "103", "100 999"), "3 invalid answers"},
		"three storages":    {answers("192.0.2.10", key, "100", "nfs-old", "nope", ""), "3 invalid answers"},
	}
	for label, c := range cases {
		h := newHost(t)
		h.stopped(t, label, h.install(t, c.input), c.want)
	}
	// an existing install is not touched either
	h := newHost(t)
	if code := h.install(t, fresh); code != 0 {
		t.Fatal(h.errb.String())
	}
	if code := h.install(t, answers("", "", "102", "", "", "", "n")); code != 1 {
		t.Fatal(code)
	}
	if text, _ := h.file(t, confFile); text != confText {
		t.Fatalf("conf changed: %q", text)
	}
}

func TestInstallAnswerRules(t *testing.T) {
	// answers to every question, the limits included: index 0 from, 1 key, 2 VMs, 3 storage,
	// 4 change limits, 5 min, 6 abort, 7 read limit, 8 workers
	base := []string{"192.0.2.10", key, "100 101", "fast-ssd", "y", "32", "8", "40", "1", "", "y"}
	blob := "ssh-ed25519 " + base64.StdEncoding.EncodeToString(append(append([]byte{}, ed25519Blob...), 1, 2, 3, 4))
	cases := []struct {
		at        int
		bad, rule string
	}{
		{0, "", "an address or pattern for authorized_keys from="},
		{0, `192.0.2.10",command="x`, "an address or pattern"},
		{0, "192.0.2.10 x", "an address or pattern"},
		{0, "a@b", "an address or pattern"},
		{1, "", "one line 'ssh-ed25519 <base64> [comment]'"},
		{1, "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQ", "one line 'ssh-ed25519"},
		{1, key + " " + key, "one line 'ssh-ed25519"},
		{1, key + "'; rm -rf /; '", "one line 'ssh-ed25519"},
		{1, `command="x" ` + key, "one line 'ssh-ed25519"},
		{1, key + " a b", "one line 'ssh-ed25519"},
		{1, blob, "one line 'ssh-ed25519 <base64> [comment]' with an OpenSSH ed25519 public key\n"},
		{2, "", "give at least one VM number"},
		{2, " , ", "give at least one VM number"},
		{2, "99", `"99" is not a VM number`},
		{2, "100.5", `"100.5" is not a VM number`},
		{2, "100 100", "VM 100 is listed twice"},
		{2, "100,999", "VM 999 is not on this host (qm list)"},
		{3, "", "the name of one of the active storages listed above"},
		{3, "nfs-old", "the name of one of the active storages listed above"},
		{3, "nope", "the name of one of the active storages listed above"},
		{3, "-x", "the name of one of the active storages listed above"},
		{4, "maybe", "answer y or n"},
		{5, "0", "FLEECING_MIN_FREE_GIB must be 1-999999 (GiB)"},
		{5, "1000000", "FLEECING_MIN_FREE_GIB must be 1-999999 (GiB)"},
		{6, "32", "FLEECING_ABORT_FREE_GIB must be below FLEECING_MIN_FREE_GIB (32)"},
		{6, "x", "FLEECING_ABORT_FREE_GIB must be 1-999999 (GiB)"},
		{7, "0", "the read limit must be a whole number of MiB/s, 1-976562"},
		{7, "1.5", "the read limit must be a whole number of MiB/s"},
		{7, "976563", "the read limit must be a whole number of MiB/s"},
		{8, "17", "MAX_WORKERS must be 1-16"},
	}
	for _, c := range cases {
		in := append(append(append([]string{}, base[:c.at]...), c.bad), base[c.at:]...)
		h := newHost(t)
		code := h.install(t, answers(in...))
		if code != 0 || strings.Count(h.out.String(), ": "+"  "+c.rule) != 1 {
			t.Errorf("%d %q: %d %q\n%s", c.at, c.bad, code, h.errb.String(), h.out.String())
		}
	}
	// a key with or without a comment stores only type and key
	for _, k := range []string{key, key + " restic-backup@backup.example"} {
		if got, err := checkPubkey(k); err != nil || got != key {
			t.Fatalf("%q: %q %v", k, got, err)
		}
	}
}

func (h *fakeHost) uninstall() int {
	h.out.Reset()
	h.errb.Reset()
	h.cmds = nil
	return h.sys.Uninstall()
}

func (h *fakeHost) installed(t *testing.T) {
	if code := h.install(t, fresh); code != 0 {
		t.Fatal(h.errb.String())
	}
	for _, p := range append([]string{lockFile}, oldFiles...) {
		_ = os.WriteFile(filepath.Join(h.root, p), []byte("x\n"), 0o644)
	}
}

func TestUninstall(t *testing.T) {
	h := newHost(t)
	h.installed(t)
	if code := h.uninstall(); code != 0 || h.errb.Len() != 0 {
		t.Fatalf("%d %s", code, h.errb.String())
	}
	want := "absent    storage restic-pve\nabsent    " + provider.PluginPath + "\nabsent    /run/restic-pve/\n" +
		"removed   /etc/systemd/system/restic-pve-update.service\nremoved   /etc/systemd/system/restic-pve-update.timer\n" +
		"removed   /etc/sudoers.d/restic-pve\nremoved   /etc/restic-pve.conf\n"
	for _, p := range oldFiles {
		want += "removed   " + p + "\n"
	}
	want += "removed   /run/restic-pve.lock\nremoved   user restic-backup and its home\nremoved   /usr/local/sbin/restic-pve\n"
	if h.out.String() != want {
		t.Fatalf("got\n%swant\n%s", h.out.String(), want)
	}
	if strings.Join(h.cmds, "\n") != "systemctl disable --now restic-pve-update.timer\nsystemctl daemon-reload\n"+
		"getent passwd restic-backup\nuserdel --remove restic-backup" {
		t.Fatalf("%q", h.cmds)
	}
	for _, p := range []string{sudoers, confFile, lockFile, Binary, units[0].path, units[1].path} {
		if _, err := os.Lstat(filepath.Join(h.root, p)); err == nil {
			t.Errorf("%s still there", p)
		}
	}
	if code := h.uninstall(); code != 0 || strings.Contains(h.out.String(), "removed") || strings.Count(h.out.String(), "absent ") != 13 ||
		!strings.Contains(h.errb.String(), "users left alone") || len(h.cmds) != 0 {
		t.Fatalf("second run %d\n%s%s", code, h.out.String(), h.errb.String())
	}
}

func TestUninstallLockHeld(t *testing.T) {
	h := newHost(t)
	h.installed(t)
	fh, err := os.Open(filepath.Join(h.root, lockFile))
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	if err := syscall.Flock(int(fh.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if code := h.uninstall(); code != 75 || h.out.Len() != 0 || len(h.cmds) != 0 {
		t.Fatalf("%d %s", code, h.errb.String())
	}
	if _, err := os.Stat(filepath.Join(h.root, sudoers)); err != nil {
		t.Fatal("sudoers removed while a dump runs")
	}
}

func TestUninstallUnknownUser(t *testing.T) {
	for _, text := range []string{"# only a comment\n", "Bad.User ALL=(root) NOPASSWD: ALL\n", "restic-backup ALL=(ALL) NOPASSWD: ALL\n"} {
		h := newHost(t)
		h.installed(t)
		_ = os.Remove(filepath.Join(h.root, sudoers)) // 0440: not writable in place
		_ = os.WriteFile(filepath.Join(h.root, sudoers), []byte(text), 0o440)
		if code := h.uninstall(); code != 0 || !strings.Contains(h.errb.String(), "backup user unknown") ||
			strings.Join(h.cmds, "\n") != "systemctl disable --now restic-pve-update.timer\nsystemctl daemon-reload" {
			t.Fatalf("%q: %d %s %q", text, code, h.errb.String(), h.cmds)
		}
		if _, err := os.Lstat(filepath.Join(h.root, Binary)); err == nil || !h.exists {
			t.Fatalf("%q: binary kept or user removed", text)
		}
	}
	h := newHost(t)
	h.sys.IsRoot = func() bool { return false }
	if code := h.uninstall(); code != 1 {
		t.Fatal("not root:", code)
	}
}

// changedReadsLeft lists what of changed reads is on the host: the plug-in file, the storage
// entry and the job folder.
func (h *fakeHost) changedReadsLeft() []string {
	var left []string
	if _, err := os.Lstat(filepath.Join(h.root, filepath.Dir(provider.PluginPath))); err == nil {
		left = append(left, filepath.Dir(provider.PluginPath))
	}
	if cfg, _ := os.ReadFile(filepath.Join(h.root, provider.StorageConfig)); strings.Contains(string(cfg), "resticpve: restic-pve") {
		left = append(left, "storage entry")
	}
	if _, err := os.Lstat(filepath.Join(h.root, provider.RunDir)); err == nil {
		left = append(left, provider.RunDir)
	}
	return left
}

// Plain questions: yes adds the plug-in (written, daemons reloaded, storage added), a
// reinstall keeps the choice with Enter, no removes both, and uninstall leaves nothing.
func TestInstallChangedReads(t *testing.T) {
	h := newHost(t)
	yes := answers("192.0.2.10", key, "100, 101", "fast-ssd", "", "y", "y")
	if code := h.install(t, yes); code != 0 {
		t.Fatal(h.errb.String())
	}
	out := h.out.String()
	for _, want := range []string{
		"Changed reads (optional). Yes adds two things to this host: the file " + provider.PluginPath + " and the storage restic-pve",
		"Answer no here, or run restic-pve uninstall, and both are removed again.\nRead only the changed parts of running guests? [y/N]: ",
		"  changed reads     on: plug-in " + provider.PluginPath + ", storage restic-pve\n",
		"written   " + provider.PluginPath + "\nreloaded  pvestatd.service pvedaemon.service pveproxy.service\nadded     storage restic-pve (pvesm add)\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	cmds := strings.Join(h.cmds, "\n")
	if !strings.HasSuffix(cmds, "systemctl enable --now restic-pve-update.timer\nperl api\nperl load\n"+
		"systemctl reload-or-try-restart pvestatd.service pvedaemon.service pveproxy.service\n"+
		"pvesm add resticpve restic-pve --content backup --nodes pve1") || !strings.HasPrefix(cmds, "qm list\npvesm status\ngetent passwd restic-backup\nperl api\napt-get") {
		t.Fatalf("commands %q", h.cmds)
	}
	if text, mode := h.file(t, provider.PluginPath); text != string(provider.Plugin) || mode != 0o644 {
		t.Fatalf("plug-in %o", mode)
	}
	// reinstall, Enter everywhere: the choice stays, nothing is run again
	if code := h.install(t, enter); code != 0 {
		t.Fatal(h.errb.String())
	}
	out = h.out.String()
	if !strings.Contains(out, "Read only the changed parts of running guests? [Y/n]: ") ||
		!strings.Contains(out, "unchanged "+provider.PluginPath+"\nunchanged storage restic-pve\n") {
		t.Fatalf("choice not kept:\n%s", out)
	}
	if c := strings.Join(h.cmds, "\n"); strings.Contains(c, "pvesm add") || strings.Contains(c, "pvesm remove") || strings.Contains(c, "try-restart") || strings.Contains(c, "perl load") {
		t.Fatalf("reinstall ran %q", h.cmds)
	}
	// no: the entry first, then the file
	if code := h.install(t, answers("", "", "", "", "", "n", "y")); code != 0 {
		t.Fatal(h.errb.String())
	}
	if out := h.out.String(); !strings.Contains(out, "removed   storage restic-pve (pvesm remove)\nremoved   "+provider.PluginPath+
		"\nremoved   /usr/share/perl5/PVE/Storage/Custom/ (empty)\nreloaded  pvestatd.service pvedaemon.service pveproxy.service\nabsent    /run/restic-pve/\n") {
		t.Fatalf("no:\n%s", out)
	}
	if left := h.changedReadsLeft(); len(left) != 0 {
		t.Fatalf("left: %v", left)
	}
	// yes again, then uninstall: nothing of it is left
	if code := h.install(t, answers("", "", "", "", "", "y", "y")); code != 0 {
		t.Fatal(h.errb.String())
	}
	_ = os.MkdirAll(filepath.Join(h.root, provider.RunDir), 0o700) // a job folder left by a crash
	_ = os.WriteFile(filepath.Join(h.root, provider.RunDir, "provider.sock"), nil, 0o600)
	if code := h.uninstall(); code != 0 {
		t.Fatal(h.errb.String())
	}
	if !strings.HasPrefix(h.out.String(), "removed   storage restic-pve (pvesm remove)\nremoved   "+provider.PluginPath+
		"\nremoved   /usr/share/perl5/PVE/Storage/Custom/ (empty)\nreloaded  pvestatd.service pvedaemon.service pveproxy.service\nremoved   /run/restic-pve/\n") {
		t.Fatalf("uninstall:\n%s", h.out.String())
	}
	if left := h.changedReadsLeft(); len(left) != 0 {
		t.Fatalf("left after uninstall: %v", left)
	}
	if !strings.HasPrefix(strings.Join(h.cmds, "\n"), "pvesm remove restic-pve\nsystemctl reload-or-try-restart") {
		t.Fatalf("uninstall order %q", h.cmds)
	}
}

// Without the feature, install adds nothing of it and runs no command for it.
func TestInstallWithoutChangedReads(t *testing.T) {
	h := newHost(t)
	if code := h.install(t, fresh); code != 0 {
		t.Fatal(h.errb.String())
	}
	if !strings.Contains(h.out.String(), "off       changed reads (no plug-in, no storage entry)\n") ||
		!strings.Contains(h.out.String(), "  changed reads     off (no plug-in, no storage entry)\n") {
		t.Fatal(h.out.String())
	}
	for _, c := range h.cmds {
		if strings.HasPrefix(c, "pvesm add") || strings.HasPrefix(c, "pvesm remove") || strings.Contains(c, "try-restart") {
			t.Fatalf("ran %q", c)
		}
	}
	if left := h.changedReadsLeft(); len(left) != 0 {
		t.Fatalf("left: %v", left)
	}
}

// A running backup holds the lock: an install that would change the plug-in or the entry
// stops before anything changes; one that changes neither goes through.
func TestInstallChangedReadsWaitsForTheLock(t *testing.T) {
	h := newHost(t)
	if code := h.install(t, fresh); code != 0 {
		t.Fatal(h.errb.String())
	}
	fh, err := os.OpenFile(filepath.Join(h.root, lockFile), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	if err := syscall.Flock(int(fh.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	conf, _ := h.file(t, confFile)
	if code := h.install(t, answers("", "", "102", "", "", "y", "y")); code != 1 || h.errb.String() !=
		"restic-pve: a backup is running on this host (/run/restic-pve.lock is held); run restic-pve install again when it has finished; nothing changed\n" {
		t.Fatalf("lock held: %d %q", code, h.errb.String())
	}
	if now, _ := h.file(t, confFile); now != conf || strings.Contains(h.out.String(), "written") {
		t.Fatal("changed under a running backup")
	}
	if left := h.changedReadsLeft(); len(left) != 0 {
		t.Fatalf("written under a running backup: %v", left)
	}
	if code := h.install(t, enter); code != 0 {
		t.Fatalf("an install without a change of changed reads is refused: %s", h.errb.String())
	}
}

// The checks before anything changes: a cluster node, a Proxmox without the provider
// interface or past the plug-in's window. "Yes" is refused, nothing is written.
func TestInstallChangedReadsRefusals(t *testing.T) {
	yes := answers("192.0.2.10", key, "100", "fast-ssd", "", "y", "y")
	for label, c := range map[string]struct {
		setup func(h *fakeHost)
		want  string
	}{
		"cluster": {func(h *fakeHost) {
			os.MkdirAll(filepath.Join(h.root, "etc/pve"), 0o755)
			os.WriteFile(filepath.Join(h.root, provider.CorosyncConfig), nil, 0o640)
		}, "this host is part of a Proxmox cluster"},
		"too old":    {func(h *fakeHost) { h.apiOut = "api 10 2 base 0\n" }, "changed reads need Proxmox VE 8.4 or later"},
		"too new":    {func(h *fakeHost) { h.apiOut = "api 27 8 base 1\n" }, "run restic-pve update, then restic-pve install again"},
		"perl fails": {func(h *fakeHost) { h.failCmd = "perl" }, "cannot ask Perl"},
	} {
		h := newHost(t)
		c.setup(h)
		code := h.install(t, yes)
		if code != 1 || !strings.HasPrefix(h.errb.String(), "restic-pve: changed reads: ") || !strings.Contains(h.errb.String(), c.want) ||
			!strings.HasSuffix(h.errb.String(), "; nothing changed\n") {
			t.Errorf("%s: %d %q", label, code, h.errb.String())
		}
		if strings.Contains(h.out.String(), "written") || len(h.changedReadsLeft()) != 0 {
			t.Errorf("%s: something changed:\n%s", label, h.out.String())
		}
	}
	// a newer Proxmox inside the window: installed, with the note
	h := newHost(t)
	h.apiOut = "api 18 8 base 1\n"
	if code := h.install(t, yes); code != 0 || !strings.Contains(h.out.String(), "note      this Proxmox (storage API 18) is newer") {
		t.Fatalf("%d\n%s", code, h.out.String())
	}
}

// pvesm remove fails during uninstall: everything else goes, the binary stays, the end says
// what is left and how to finish, exit 1; the next run finishes.
func TestUninstallWithAStorageEntryThatStays(t *testing.T) {
	h := newHost(t)
	if code := h.install(t, answers("192.0.2.10", key, "100", "fast-ssd", "", "y", "y")); code != 0 {
		t.Fatal(h.errb.String())
	}
	h.failCmd = "pvesm remove"
	if code := h.uninstall(); code != 1 || !strings.Contains(h.errb.String(),
		"uninstall not complete: left on this host: the storage restic-pve and "+provider.PluginPath+". To finish: pvesm remove restic-pve") {
		t.Fatalf("%d %s", code, h.errb.String())
	}
	for _, p := range []string{sudoers, confFile, units[1].path} {
		if _, err := os.Lstat(filepath.Join(h.root, p)); err == nil {
			t.Errorf("%s still there", p)
		}
	}
	if _, err := os.Stat(filepath.Join(h.root, Binary)); err != nil || !strings.Contains(h.out.String(), "kept      "+Binary) {
		t.Fatal("the binary must stay to finish")
	}
	h.failCmd = ""
	if code := h.uninstall(); code != 0 || len(h.changedReadsLeft()) != 0 {
		t.Fatalf("second run %d %s %v", code, h.errb.String(), h.changedReadsLeft())
	}
}
