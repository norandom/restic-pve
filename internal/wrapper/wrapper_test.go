package wrapper

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The test binary acts as the wrapper when RESTICPVE_TEST_DIR is set (helper process).
func TestMain(m *testing.M) {
	if dir := os.Getenv("RESTICPVE_TEST_DIR"); dir != "" {
		e := Default("v9.9.9")
		e.ConfFile = filepath.Join(dir, "restic-pve.conf")
		e.LockFile = filepath.Join(dir, "restic-pve.lock")
		e.Path = filepath.Join(dir, "bin") + ":/usr/bin:/bin"
		if p, err := time.ParseDuration(os.Getenv("RESTICPVE_TEST_POLL")); err == nil {
			e.SpacePoll = p
		}
		os.Exit(e.Run(os.Args[1:]))
	}
	os.Exit(m.Run())
}

const (
	testConf = "FLEECING_STORAGE=fast-ssd\nFLEECING_MIN_FREE_GIB=32\nFLEECING_ABORT_FREE_GIB=8\nBWLIMIT_KIB=40960\nMAX_WORKERS=1\nVMIDS=100 101 103\n"
	vm100    = `agent: 1,fstrim_cloned_disks=1
boot: order=scsi0
cores: 2
efidisk0: fast-ssd:100/vm-100-disk-1.qcow2,efitype=4m,size=528K
ide2: none,media=cdrom
memory: 4096
name: example
net0: virtio=BC:24:11:00:00:01,bridge=vmbr0
ostype: l26
scsi0: fast-ssd:100/vm-100-disk-0.qcow2,iothread=1,size=32G
scsihw: virtio-scsi-single
tpmstate0: fast-ssd:100/vm-100-disk-2.raw,size=4M,version=v2.0
unused0: fast-ssd:100/vm-100-disk-9.qcow2
`
	vm101 = `agent: 0
lock: backup
name: example-win
ostype: win11
sata1: fast-ssd:101/vm-101-disk-0.qcow2,size=8G
virtio0: fast-ssd:101/vm-101-disk-1.qcow2,size=16G
vmstate: fast-ssd:101/vm-101-state-hib.raw
`
	pvever = "pve-manager/9.0.3/0123456789abcdef (running kernel: 6.14.8-2-pve)\n"
)

func pvesm(availKiB int64, name, status string) string {
	return "Name             Type     Status           Total            Used       Available        %\n" +
		name + "          dir     " + status + "       959786032       100000000       " +
		itoa(availKiB) + "   10.42%\n"
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// Stubs write their calls into the state dir S (baked in: children get no test environment).
var stubs = map[string]string{
	"qm": `echo "qm $*" >> "$S/qm.log"
case "$1" in
  config)
    if [ -f "$S/vm-$2.conf" ]; then cat "$S/vm-$2.conf"; exit 0; fi
    if [ -f "$S/qm-config-broken" ]; then echo "ipcc_send_rec failed: connection refused" >&2; exit 255; fi
    echo "Configuration file 'nodes/pve/qemu-server/$2.conf' does not exist" >&2; exit 2 ;;
  status) echo "status: $(cat "$S/vm-$2.status")" ;;
  guest) [ "$2" = cmd ] && [ "$4" = ping ] || exit 9
         exit "$(cat "$S/vm-$3.agent" 2>/dev/null || echo 1)" ;;
  *) exit 9 ;;
esac
`,
	"pvesm": `echo "pvesm $*" >> "$S/pvesm.log"
if [ "$1" = list ]; then
  [ "$2" = fast-ssd ] && [ "$3" = --vmid ] || exit 9
  [ ! -f "$S/pvesm-list-failed" ] || exit 255
  cat "$S/pvesm-list-$4.txt"; exit $?
fi
[ "$1" = status ] && [ "$2" = --storage ] || exit 9
n=$(grep -c . "$S/pvesm.log")
if [ "$n" -gt 1 ] && [ -f "$S/pvesm-$3.later.txt" ]; then cat "$S/pvesm-$3.later.txt"; exit 0; fi
[ -f "$S/pvesm-$3.txt" ] || { echo "storage '$3' does not exist" >&2; exit 255; }
cat "$S/pvesm-$3.txt"
`,
	"pveversion": `cat "$S/pveversion"
`,
	"vzdump": `for a in "$@"; do printf '%s\n' "$a"; done > "$S/vzdump.argv"
nice > "$S/vzdump.nice"
env > "$S/vzdump.env"
readlink /proc/$$/fd/3 > "$S/vzdump.fd3"
printf 'VMA-STREAM'
echo "INFO: starting new backup job: vzdump $1" >&2
touch "$S/vzdump.started"
if [ -f "$S/vzdump.sleep" ]; then sleep "$(cat "$S/vzdump.sleep")" > /dev/null 2>&1 & wait $!; fi
exit "$(cat "$S/vzdump.rc" 2>/dev/null || echo 0)"
`,
}

type fixture struct {
	t                *testing.T
	dir, state, home string
	poll             string
}

type result struct {
	code           int
	stdout, stderr string
}

func newFixture(t *testing.T) *fixture {
	dir := t.TempDir()
	f := &fixture{t: t, dir: dir, state: filepath.Join(dir, "state"), home: filepath.Join(dir, "home"), poll: "15s"}
	for _, d := range []string{"bin", "state", "home"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range stubs {
		f.writeFile(filepath.Join(dir, "bin", name), "#!/bin/sh\nS="+f.state+"\n"+body, 0o755)
	}
	f.writeFile(filepath.Join(dir, "restic-pve.conf"), testConf, 0o644)
	for name, text := range map[string]string{
		"vm-100.conf": vm100, "vm-100.status": "running", "vm-100.agent": "0",
		"vm-101.conf": vm101, "vm-101.status": "stopped", "pvesm-fast-ssd.txt": pvesm(859786032, "fast-ssd", "active"),
		"pvesm-list-100.txt": "Volid Format Type Size VMID\n", "pveversion": pvever,
	} {
		f.set(name, text)
	}
	return f
}

func (f *fixture) writeFile(path, text string, mode os.FileMode) {
	f.t.Helper()
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) set(name, text string) { f.writeFile(filepath.Join(f.state, name), text, 0o644) }
func (f *fixture) conf(text string) {
	f.writeFile(filepath.Join(f.dir, "restic-pve.conf"), text, 0o644)
}
func (f *fixture) rm(name string) { _ = os.Remove(filepath.Join(f.state, name)) }

func (f *fixture) read(name string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(f.state, name))
	return string(b), err == nil
}

// cmd runs the wrapper like sshd runs the forced command: the login shell expands
// $SSH_ORIGINAL_COMMAND unquoted (word splitting, no command substitution).
func (f *fixture) cmd(request string) *exec.Cmd {
	exe, err := os.Executable()
	if err != nil {
		f.t.Fatal(err)
	}
	c := exec.Command("/bin/sh", "-c", `exec "$0" $SSH_ORIGINAL_COMMAND`, exe)
	c.Dir = f.home
	c.Env = []string{"PATH=/usr/bin:/bin", "SSH_ORIGINAL_COMMAND=" + request,
		"RESTICPVE_TEST_DIR=" + f.dir, "RESTICPVE_TEST_POLL=" + f.poll}
	return c
}

func (f *fixture) run(c *exec.Cmd) result {
	f.t.Helper()
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	err := c.Run()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		f.t.Fatal(err)
	}
	return result{c.ProcessState.ExitCode(), out.String(), errb.String()}
}

func (f *fixture) req(request string) result { return f.run(f.cmd(request)) }

func (f *fixture) args(args ...string) result {
	exe, _ := os.Executable()
	c := exec.Command(exe, args...)
	c.Dir = f.home
	c.Env = []string{"RESTICPVE_TEST_DIR=" + f.dir}
	return f.run(c)
}

func (f *fixture) info(request string) map[string]any {
	f.t.Helper()
	r := f.req(request)
	var m map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &m); r.code != 0 || err != nil {
		f.t.Fatalf("%s: %d %q %v", request, r.code, r.stderr, err)
	}
	return m
}

func (f *fixture) noVzdump() {
	f.t.Helper()
	if _, ok := f.read("vzdump.argv"); ok {
		f.t.Fatal("vzdump was started")
	}
}

func TestPing(t *testing.T) {
	if r := newFixture(t).req("ping"); r.code != 0 || r.stdout != "ok\n" {
		t.Fatalf("%+v", r)
	}
}

func TestDumpExactArgv(t *testing.T) {
	f := newFixture(t)
	for _, vmid := range []string{"100", "101", "103"} {
		f.rm("vzdump.argv")
		r := f.req("dump " + vmid)
		if r.code != 0 || r.stdout != "VMA-STREAM" || !strings.Contains(r.stderr, "starting new backup job") {
			t.Fatalf("%s: %+v", vmid, r)
		}
		argv, _ := f.read("vzdump.argv")
		want := vmid + "\n--mode\nsnapshot\n--stdout\n--compress\n0\n--bwlimit\n40960\n--ionice\n7\n" +
			"--performance\nmax-workers=1\n--fleecing\nenabled=1,storage=fast-ssd\n"
		if argv != want {
			t.Fatalf("argv %q", argv)
		}
		if n, _ := f.read("vzdump.nice"); strings.TrimSpace(n) != "19" {
			t.Fatalf("nice %q", n)
		}
		if fd, _ := f.read("vzdump.fd3"); strings.TrimSpace(fd) != filepath.Join(f.dir, "restic-pve.lock") {
			t.Fatalf("vzdump does not hold the lock: %q", fd)
		}
		env, _ := f.read("vzdump.env")
		if !strings.Contains(env, "LC_ALL=C\n") || strings.Contains(env, "RESTICPVE") || strings.Contains(env, "SSH_ORIGINAL") {
			t.Fatalf("child environment %q", env)
		}
	}
}

func TestDumpValuesComeFromConf(t *testing.T) {
	f := newFixture(t)
	f.conf("# tuned\nMAX_WORKERS=16\nBWLIMIT_KIB=10240\n\nFLEECING_STORAGE=scratch.1\nFLEECING_ABORT_FREE_GIB=1\nFLEECING_MIN_FREE_GIB=2\nVMIDS=100\n")
	f.set("pvesm-scratch.1.txt", pvesm(3*1048576, "scratch.1", "active"))
	if r := f.req("dump 100"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	argv, _ := f.read("vzdump.argv")
	for _, want := range []string{"\n10240\n", "\nenabled=1,storage=scratch.1\n", "\nmax-workers=16\n"} {
		if !strings.Contains(argv, want) {
			t.Fatalf("%q lacks %q", argv, want)
		}
	}
}

func TestDumpExitStatusIsVzdumps(t *testing.T) {
	f := newFixture(t)
	f.set("vzdump.rc", "5")
	if r := f.req("dump 100"); r.code != 5 {
		t.Fatalf("%+v", r)
	}
	f.rm("vzdump.rc")
	for i := 0; i < 2; i++ { // the lock is released after each run
		if r := f.req("dump 100"); r.code != 0 {
			t.Fatalf("run %d: %+v", i, r)
		}
	}
}

func TestInfoJSON(t *testing.T) {
	f := newFixture(t)
	r := f.req("info 100")
	want := `{"vmid":100,"name":"example","status":"running","lock":null,"ostype":"l26","agent_enabled":true,` +
		`"agent_running":true,"disks":["efidisk0","scsi0","tpmstate0"],"vmstate_present":false,"fleecing_ok":true,` +
		`"fleecing_avail_bytes":880420896768,"fleecing_leftovers":[],"fleecing_in_config":false,` +
		`"conf":{"fleecing_storage":"fast-ssd","fleecing_min_free_gib":"32","fleecing_abort_free_gib":"8",` +
		`"bwlimit_kib":"40960","max_workers":"1"},"pve_version":"9.0.3","version":"v9.9.9"}` + "\n"
	if r.code != 0 || r.stdout != want {
		t.Fatalf("%d %q\n got %s\nwant %s", r.code, r.stderr, r.stdout, want)
	}
	qm, _ := f.read("qm.log")
	if qm != "qm config 100\nqm status 100\nqm guest cmd 100 ping\n" {
		t.Fatalf("qm calls %q", qm)
	}
	if log, _ := f.read("pvesm.log"); log != "pvesm status --storage fast-ssd\npvesm list fast-ssd --vmid 100\n" {
		t.Fatalf("pvesm calls %q", log)
	}
	f.noVzdump()
}

func TestInfoLockedVmstateAgentOff(t *testing.T) {
	f := newFixture(t)
	got := f.info("info 101")
	for k, v := range map[string]any{"lock": "backup", "status": "stopped", "agent_enabled": false, "agent_running": false,
		"vmstate_present": true, "disks": []any{"sata1", "virtio0"}, "ostype": "win11"} {
		if !reflect.DeepEqual(got[k], v) {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if qm, _ := f.read("qm.log"); strings.Contains(qm, "guest") {
		t.Fatal("agent disabled but asked")
	}
}

func TestInfoAgentNotRunning(t *testing.T) {
	f := newFixture(t)
	f.set("vm-100.agent", "1")
	if got := f.info("info 100"); got["agent_enabled"] != true || got["agent_running"] != false {
		t.Fatalf("%v", got)
	}
}

func TestInfoFleecingLeftovers(t *testing.T) {
	f := newFixture(t)
	hits := []string{"fast-ssd:100/vm-100-fleece-0.qcow2", "fast-ssd:100/vm-100-fleece-12.raw", "fast-ssd:vm-100-fleece-3"}
	other := []string{"fast-ssd:100/vm-100-disk-0.qcow2", "fast-ssd:101/vm-101-fleece-0.qcow2",
		"fast-ssd:100/vm-100-fleece-1-extra.raw", "fast-ssd:100/vm-100-fleece-x.raw"}
	list := "Volid Format Type Size VMID\n"
	for _, v := range append(append([]string{}, hits...), other...) {
		list += v + " raw images 4096 100\n"
	}
	f.set("pvesm-list-100.txt", list)
	got := f.info("info 100")
	if !reflect.DeepEqual(got["fleecing_leftovers"], []any{hits[0], hits[1], hits[2]}) || got["fleecing_in_config"] != false {
		t.Fatalf("%v", got)
	}
	f.set("pvesm-list-failed", "")
	if got := f.info("info 100"); got["fleecing_leftovers"] != nil {
		t.Fatalf("list failure: %v", got["fleecing_leftovers"])
	}
	f.noVzdump()
}

func TestInfoFleecingInConfig(t *testing.T) {
	f := newFixture(t)
	for _, extra := range []string{"[special:fleecing]\nscsi0: fast-ssd:100/vm-100-fleece-0.qcow2\n",
		"fleecing: fast-ssd:100/vm-100-fleece-0.qcow2\n"} {
		f.set("vm-100.conf", vm100+extra)
		if got := f.info("info 100"); got["fleecing_in_config"] != true {
			t.Fatalf("%q: %v", extra, got)
		}
	}
	f.noVzdump()
}

func TestInfoFleecingOK(t *testing.T) {
	f := newFixture(t)
	good := pvesm(859786032, "fast-ssd", "active")
	cases := []struct {
		label, ver, storage string
		ok                  bool
	}{
		{"old PVE", "pve-manager/8.1.10/abc (running kernel: 6.5)\n", good, false},
		{"inactive", pvever, pvesm(859786032, "fast-ssd", "inactive"), false},
		{"unknown storage", pvever, "", false},
		{"unparsable version", "something else\n", good, false},
		{"8.2 is enough", "pve-manager/8.2.4/faa83925 (running kernel: 6.8.4-2-pve)\n", good, true},
	}
	for _, c := range cases {
		f.set("pveversion", c.ver)
		f.rm("pvesm-fast-ssd.txt")
		if c.storage != "" {
			f.set("pvesm-fast-ssd.txt", c.storage)
		}
		got := f.info("info 100")
		if got["fleecing_ok"] != c.ok {
			t.Errorf("%s: fleecing_ok %v", c.label, got["fleecing_ok"])
		}
		if c.storage == "" && got["fleecing_avail_bytes"] != nil {
			t.Errorf("unknown storage reports %v", got["fleecing_avail_bytes"])
		}
		if c.ok && got["pve_version"] != "8.2.4" {
			t.Errorf("pve_version %v", got["pve_version"])
		}
	}
}

func TestInfoEscapesStrings(t *testing.T) {
	f := newFixture(t)
	f.set("vm-100.conf", strings.Replace(vm100, "name: example", "name: ex\"am\\ple\x01\x1f", 1))
	if got := f.info("info 100"); got["name"] != `ex"am\ple` {
		t.Fatalf("%q", got["name"])
	}
}

func TestInfoMissingVM(t *testing.T) {
	f := newFixture(t)
	f.rm("vm-100.conf")
	if r := f.req("info 100"); r.code != 2 || r.stdout != "" {
		t.Fatalf("%+v", r)
	}
	f.set("qm-config-broken", "")
	if r := f.req("info 100"); r.code != 1 {
		t.Fatalf("other qm failure: %+v", r)
	}
}

func lockFile(t *testing.T, f *fixture) *os.File {
	fh, err := os.Create(filepath.Join(f.dir, "restic-pve.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(fh.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	return fh
}

func TestBusyLock(t *testing.T) {
	f := newFixture(t)
	fh := lockFile(t, f)
	defer fh.Close()
	if r := f.req("dump 100"); r.code != 75 || !strings.Contains(r.stderr, "restic-pve: error: busy") {
		t.Fatalf("%+v", r)
	}
	f.noVzdump()
	for _, req := range []string{"ping", "info 100"} {
		if r := f.req(req); r.code != 0 {
			t.Fatalf("%s blocked by the lock: %+v", req, r)
		}
	}
}

var refused = []string{
	"dump 100 nofleecing", "", "bash", "sh -i", "/bin/sh", "bash -c id",
	"scp -t /tmp", "scp -f /etc/shadow", "/usr/lib/openssh/sftp-server", "internal-sftp",
	"qm list", "qm start 100", "qm config 100", "qm guest exec 100 id", "vzdump 100 --stdout", "pvesm status",
	"dump 102", "info 102", "dump 999999999",
	"dump", "info", "ping x", "stat", "stat x", "dump 100 extra", "dump 100 100",
	"dump 100 --mode stop", "dump 100 --bwlimit 0", "info 100 --verbose",
	"dump 100;id", "dump 100; id", "ping;id", "dump 100&&id", "dump 100|id", "dump 100>x",
	"dump $(id)", "dump `id`", "dump 10*", "dump 1?0", "dump [1]00", "dump ~",
	"dump abc", "dump -1", "dump 0100", "dump 1e2", "dump 99", "dump +100", "dump 100.0",
	"PING", "help", "--help", "ping\tx",
	"snap-create 100", "snap-delete 100", "disks 100", "config 100", "read 100 scsi0",
	"install x", "update", "version", "request ping", "uninstall",
}

func TestRefusalChecklist(t *testing.T) {
	f := newFixture(t)
	for _, req := range refused {
		if r := f.req(req); r.code != 64 || r.stdout != "" || !strings.Contains(r.stderr, "restic-pve: refused: ") {
			t.Errorf("%q: %+v", req, r)
		}
	}
	for _, args := range [][]string{{"dump", "100 "}, {"dump", " 100"}, {"dump", "100\n"}, {"dump", ""}, {"", ""},
		{"ping", ""}, {"info", "100", ""}} {
		if r := f.args(args...); r.code != 64 {
			t.Errorf("%q: %+v", args, r)
		}
	}
	f.noVzdump()
	if _, ok := f.read("qm.log"); ok {
		t.Fatal("a refused request reached qm")
	}
	if ents, _ := os.ReadDir(f.home); len(ents) != 0 {
		t.Fatalf("a request created files: %v", ents)
	}
}

func TestAllowList(t *testing.T) {
	f := newFixture(t)
	without := strings.Replace(testConf, "VMIDS=100 101 103\n", "", 1)
	f.conf(without + "# VMIDS=100\nVMIDS=101\n")
	if r := f.req("dump 100"); r.code != 64 || !strings.Contains(r.stderr, "restic-pve: refused: VM 100 is not on the allow list") {
		t.Fatalf("comment is an entry: %+v", r)
	}
	for _, line := range []string{"VMIDS=\n", "VMIDS=100  101\n", "VMIDS= 100\n", "VMIDS=100 \n", "VMIDS=100,101\n",
		"VMIDS=abc\n", "VMIDS=0100\n", "VMIDS=99\n", "VMIDS=101 100 101\n", "VMIDS=100\tx\n", ""} {
		f.conf(without + line)
		for _, req := range []string{"dump 100", "info 101"} {
			if r := f.req(req); r.code != 78 || !strings.Contains(r.stderr, "restic-pve.conf") {
				t.Errorf("%q %s: %+v", line, req, r)
			}
		}
	}
	f.conf(without + "VMIDS=101 100 101\n")
	if r := f.req("dump 100"); !strings.Contains(r.stderr, "line 6: VMIDS: VM 101 listed twice") {
		t.Fatalf("%+v", r)
	}
	f.conf(without + "VMIDS=999999999 100\n")
	if r := f.req("dump 100"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	_ = os.Remove(filepath.Join(f.dir, "restic-pve.conf"))
	for _, req := range []string{"dump 100", "info 100"} {
		if r := f.req(req); r.code != 78 || !strings.Contains(r.stderr, "restic-pve.conf missing (run restic-pve install again)") {
			t.Fatalf("%s: %+v", req, r)
		}
	}
}

func TestBadConfFailsClosed(t *testing.T) {
	f := newFixture(t)
	marker := filepath.Join(f.dir, "pwned")
	bad := []string{
		"FLEECING_STORAGE=fast-ssd\nBWLIMIT_KIB=40960\n", testConf + "EXTRA=1\n", testConf + "MAX_WORKERS=2\n",
		testConf + "MAX_WORKERS 2\n", "FLEECING_STORAGE = fast-ssd\nBWLIMIT_KIB=40960\nMAX_WORKERS=1\n",
		"FLEECING_STORAGE=\"fast-ssd\"\n", "FLEECING_STORAGE=-x\n", "BWLIMIT_KIB=0\n", "MAX_WORKERS=0\n", "MAX_WORKERS=17\n",
		strings.Replace(testConf, "MIN_FREE_GIB=32", "MIN_FREE_GIB=0", 1),
		strings.Replace(testConf, "ABORT_FREE_GIB=8", "ABORT_FREE_GIB=8G", 1),
		strings.Replace(testConf, "FLEECING_ABORT_FREE_GIB=8\n", "", 1),
		"fleecing_storage=fast-ssd\n", "FLEECING_STORAGE=$(touch " + marker + ")\n", "touch " + marker + "\n",
		"BWLIMIT_KIB=1; touch " + marker + "\n",
	}
	for _, text := range bad {
		f.conf(text)
		for _, req := range []string{"dump 100", "info 100"} {
			if r := f.req(req); r.code != 78 || !strings.Contains(r.stderr, "restic-pve.conf") {
				t.Errorf("%q %s: %+v", text, req, r)
			}
		}
	}
	f.conf(strings.Replace(testConf, "ABORT_FREE_GIB=8", "ABORT_FREE_GIB=32", 1))
	if r := f.req("dump 100"); r.code != 78 || !strings.Contains(r.stderr, "FLEECING_ABORT_FREE_GIB must be below FLEECING_MIN_FREE_GIB") {
		t.Fatalf("%+v", r)
	}
	_ = os.Remove(filepath.Join(f.dir, "restic-pve.conf"))
	if r := f.req("dump 100"); r.code != 78 || !strings.Contains(r.stderr, "missing (run restic-pve install again)") {
		t.Fatalf("%+v", r)
	}
	if r := f.req("ping"); r.code != 0 {
		t.Fatalf("ping needs no conf: %+v", r)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("conf was executed")
	}
	f.noVzdump()
}

func TestFreeSpacePrecheck(t *testing.T) {
	f := newFixture(t)
	f.set("pvesm-fast-ssd.txt", pvesm(31*1048576, "fast-ssd", "active"))
	if r := f.req("dump 100"); r.code != 73 || !strings.Contains(r.stderr, "31 GiB free, below FLEECING_MIN_FREE_GIB=32; dump not started") {
		t.Fatalf("%+v", r)
	}
	for _, text := range []string{"", pvesm(900*1048576, "fast-ssd", "inactive")} {
		f.rm("pvesm-fast-ssd.txt")
		if text != "" {
			f.set("pvesm-fast-ssd.txt", text)
		}
		if r := f.req("dump 100"); r.code != 73 || !strings.Contains(r.stderr, "free space unknown") {
			t.Fatalf("%+v", r)
		}
	}
	f.noVzdump()
	f.set("pvesm-fast-ssd.txt", pvesm(32*1048576, "fast-ssd", "active"))
	if r := f.req("dump 100"); r.code != 0 {
		t.Fatalf("exactly the limit: %+v", r)
	}
}

func TestLowSpaceDuringDumpStopsVzdump(t *testing.T) {
	f := newFixture(t)
	f.poll = "200ms"
	f.set("pvesm-fast-ssd.later.txt", pvesm(7*1048576, "fast-ssd", "active"))
	f.set("vzdump.sleep", "20")
	start := time.Now()
	r := f.req("dump 100")
	if r.code != 73 || !strings.Contains(r.stderr, "restic-pve: error: fleecing storage fast-ssd has 7 GiB free, below FLEECING_ABORT_FREE_GIB=8; stopping vzdump") {
		t.Fatalf("%+v", r)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("vzdump was not stopped")
	}
}

func TestEnoughSpaceKeepsVzdumpStatus(t *testing.T) {
	f := newFixture(t)
	f.poll = "200ms"
	f.set("vzdump.sleep", "1")
	for _, rc := range []string{"0", "5"} {
		f.set("vzdump.rc", rc)
		if r := f.req("dump 100"); itoa(int64(r.code)) != rc {
			t.Fatalf("rc %s: %+v", rc, r)
		}
	}
}

// Signals to the wrapper reach vzdump as SIGTERM; the wrapper exits with vzdump's status
// (143: vzdump ended by SIGTERM). The lock stays held while vzdump runs.
func TestSignalForwarding(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT, syscall.SIGPIPE} {
		f := newFixture(t)
		f.set("vzdump.sleep", "30")
		c := f.cmd("dump 100")
		var errb bytes.Buffer
		c.Stderr = &errb
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, ok := f.read("vzdump.started"); ok {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("vzdump did not start")
			}
			time.Sleep(20 * time.Millisecond)
		}
		if r := f.req("dump 100"); r.code != 75 {
			t.Fatalf("second dump while one runs: %+v", r)
		}
		_ = c.Process.Signal(sig)
		_ = c.Wait()
		if code := c.ProcessState.ExitCode(); code != 143 {
			t.Fatalf("%v: exit %d, stderr %q", sig, code, errb.String())
		}
	}
}
