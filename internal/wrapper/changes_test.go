package wrapper

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/norandom/restic-pve/internal/nbd/nbdtest"
	"github.com/norandom/restic-pve/internal/pieces"
	"github.com/norandom/restic-pve/internal/provider"
	"github.com/norandom/restic-pve/internal/stream/changes"
	"github.com/norandom/restic-pve/internal/testfix"
)

const storageCfg = "dir: local\n\tpath /var/lib/vz\n\tcontent iso,backup\n\nresticpve: restic-pve\n\tcontent backup\n\tnodes pve1\n"

func (f *fixture) root(p string) string { return filepath.Join(f.dir, "root", p) }

// enable puts the plug-in and the storage entry on the fake host, as install with yes does.
func (f *fixture) enable() {
	f.t.Helper()
	for p, text := range map[string]string{provider.PluginPath: string(provider.Plugin), provider.StorageConfig: storageCfg} {
		if err := os.MkdirAll(filepath.Dir(f.root(p)), 0o755); err != nil {
			f.t.Fatal(err)
		}
		f.writeFile(f.root(p), text, 0o644)
	}
	f.set("pvesm-restic-pve.txt", pvesm(0, "restic-pve", "active"))
}

// providerVzdump replaces the vzdump stub with one that, for "--storage restic-pve", does what
// vzdump does for a provider job: it opens a fake NBD export of the spec's disks under
// qemu-server's folder and runs the Perl stand-in, which loads the real plug-in file from the
// fake host and calls it in vzdump's order. The dump branch stays as before.
func (f *fixture) providerVzdump(spec nbdtest.Spec) {
	f.t.Helper()
	perl := testfix.Perl(f.t)
	exe, _ := os.Executable()
	driver, _ := filepath.Abs("../provider/testdata/vzdump-driver.pl")
	stubs, _ := filepath.Abs("../provider/testdata/lib")
	spec.ExitStatus = true
	b, _ := json.Marshal(spec)
	f.set("spec.json", string(b))
	lib := f.root("usr/share/perl5")
	script := "#!/bin/sh\nS=" + f.state + "\n" + `for a in "$@"; do printf '%s\n' "$a"; done > "$S/vzdump.argv"
if [ "$2" != --storage ]; then
  printf 'VMA-STREAM'
  exit 0
fi
vmid=$1
echo "INFO: starting new backup job: vzdump $*" >&2
touch "$S/vzdump.started"
if [ -f "$S/vzdump.sleep" ]; then sleep "$(cat "$S/vzdump.sleep")" > /dev/null 2>&1 & wait $!; fi
if [ -f "$S/vzdump.rc" ]; then echo "ERROR: Backup of VM $vmid failed - fake failure" >&2; exit "$(cat "$S/vzdump.rc")"; fi
dir=` + filepath.Join(f.dir, "qemu-server") + `/$vmid.backup-access.$$
mkdir -p "$dir"; chmod 700 "$dir"
sock="$dir/$vmid-nbd.backup-access"
RESTICPVE_TEST_NBD=1 ` + exe + ` "$sock" "$S/spec.json" snapshot-access:restic-pve &
nbd=$!
i=0; while [ ! -S "$sock" ] && [ $i -lt 400 ]; do sleep 0.05; i=$((i+1)); done
` + perl + ` -I ` + lib + ` -I ` + stubs + ` ` + driver + ` job ` + lib + ` ` + f.root(provider.RunDir) + `/provider.sock $vmid "$S/spec.json" "$dir" >&2 &
drv=$!
# vzdump stopped by a signal: its worker ends, the export closes
trap 'kill $drv $nbd 2>/dev/null; wait $drv; rm -rf "$dir"; exit 143' TERM INT
wait $drv
rc=$?
kill $nbd; wait $nbd
cp "$sock.log" "$S/nbd.log" 2>/dev/null
rm -rf "$dir"
exit $rc
`
	f.writeFile(filepath.Join(f.dir, "bin", "vzdump"), script, 0o755)
}

// jobSpec: a 64 MiB disk and a small EFI disk, both with a record unless said otherwise.
func jobSpec(record bool, dirty ...[2]uint64) nbdtest.Spec {
	return nbdtest.Spec{
		Config:   "boot: order=scsi0\ncores: 2\nname: example\nscsi0: fast-ssd:100/vm-100-disk-0.qcow2,size=64M\n",
		Firewall: "[OPTIONS]\nenable: 1\n",
		Devices: []nbdtest.SpecDevice{
			{Name: "drive-efidisk0", Size: 540672, Record: record},
			{Name: "drive-scsi0", Size: 64 << 20, Record: record, Dirty: dirty},
		},
	}
}

// stream is what the backup machine saw of one changes request.
type stream struct {
	h       *changes.Header
	configs map[string]string
	maps    map[string]changes.Map
	pieces  map[string][]uint64 // device: offsets
	data    map[string][]byte   // "<device> <offset>": bytes
	end     *changes.End
	err     error // decoding stopped
	result
}

// job runs a changes request like the backup machine: it reads the frames and, after the
// end frame, writes ack(header) (no line at all for "") and closes stdin. With cut, it closes
// both pipes right after the header frame (the ssh connection is lost).
func (f *fixture) job(request string, cut bool, ack func(h *changes.Header) string) stream {
	f.t.Helper()
	c := f.cmd(request)
	c.WaitDelay = 20 * time.Second
	stdin, _ := c.StdinPipe()
	stdout, _ := c.StdoutPipe()
	var errb bytes.Buffer
	c.Stderr = &errb
	if err := c.Start(); err != nil {
		f.t.Fatal(err)
	}
	s := stream{configs: map[string]string{}, maps: map[string]changes.Map{}, pieces: map[string][]uint64{}, data: map[string][]byte{}}
	names := map[uint8]string{}
	dec := changes.NewDecoder(stdout)
	for {
		fr, err := dec.Next()
		if err != nil {
			s.err = err
			break
		}
		switch {
		case fr.Header != nil:
			s.h = fr.Header
			for _, d := range fr.Header.Devices {
				names[d.ID] = d.Name
			}
		case fr.Config != nil:
			s.configs[fr.Config.Name] = string(fr.Config.Data)
		case fr.Map != nil:
			s.maps[names[fr.Map.Dev]] = *fr.Map
		case fr.Piece != nil:
			n := names[fr.Piece.Dev]
			s.pieces[n] = append(s.pieces[n], fr.Piece.Offset)
			s.data[fmt.Sprintf("%s %d", n, fr.Piece.Offset)] = append([]byte(nil), fr.Piece.Data...)
		case fr.End != nil:
			s.end = fr.End
		}
		if cut && s.h != nil {
			stdout.Close()
			stdin.Close()
			break
		}
		if s.end != nil {
			if line := ack(s.h); line != "" {
				io.WriteString(stdin, line)
			}
			stdin.Close()
			s.err = io.EOF
			if err := dec.CheckEOF(); err != nil {
				s.err = err
			}
			io.Copy(io.Discard, stdout)
			break
		}
	}
	stdin.Close()
	err := c.Wait()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		f.t.Fatal(err)
	}
	s.result = result{c.ProcessState.ExitCode(), "", errb.String()}
	return s
}

func ackLine(v changes.Verb) func(h *changes.Header) string {
	return func(h *changes.Header) string {
		a := changes.Ack{Token: h.Token, Verb: v}
		switch v {
		case changes.VerbOK:
			a.Snapshot = strings.Repeat("ab", 32)
		case changes.VerbFail:
			a.Reason = "check C2 failed"
		}
		line, _ := a.Line()
		return line
	}
}

// clean checks that a job left the host as it was: no socket and nothing in the job folder
// but the guests' last-job files, the storage entry untouched, the plug-in untouched, no
// qemu-server folder of ours, and the lock free.
func (f *fixture) clean(label string) {
	f.t.Helper()
	ents, _ := os.ReadDir(f.root(provider.RunDir))
	for _, e := range ents {
		if !regexp.MustCompile(`^job-[0-9]+$`).MatchString(e.Name()) {
			f.t.Errorf("%s: left in the job folder: %s", label, e.Name())
		}
	}
	if b, _ := os.ReadFile(f.root(provider.StorageConfig)); string(b) != storageCfg {
		f.t.Errorf("%s: storage.cfg changed: %q", label, b)
	}
	if b, _ := os.ReadFile(f.root(provider.PluginPath)); !bytes.Equal(b, provider.Plugin) {
		f.t.Errorf("%s: plug-in changed", label)
	}
	if ents, _ := os.ReadDir(filepath.Join(f.dir, "qemu-server")); len(ents) != 0 {
		f.t.Errorf("%s: left below qemu-server: %v", label, ents)
	}
	if ents, _ := os.ReadDir(f.home); len(ents) != 0 {
		f.t.Errorf("%s: files in the cwd: %v", label, ents)
	}
	fh, err := os.OpenFile(filepath.Join(f.dir, "restic-pve.lock"), os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		f.t.Fatal(err)
	}
	defer fh.Close()
	if err := syscall.Flock(int(fh.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.t.Errorf("%s: lock still held", label)
	}
	syscall.Flock(int(fh.Fd()), syscall.LOCK_UN)
}

func (f *fixture) nbdLog() string { s, _ := f.read("nbd.log"); return s }

const piece = pieces.PieceSize

func TestChangesProbe(t *testing.T) {
	f := newFixture(t)
	f.enable()
	f.providerVzdump(jobSpec(true, [2]uint64{0, piece}, [2]uint64{4 * piece, 2 * piece}))
	s := f.job("changes 100 probe", false, ackLine(changes.VerbProbe))
	if s.code != 0 || s.err != io.EOF {
		t.Fatalf("%d %v\n%s", s.code, s.err, s.stderr)
	}
	if s.h.Mode != "probe" || s.h.VMID != 100 || s.h.PVE != "9.0.3" || len(s.h.Devices) != 2 ||
		s.h.Devices[0].Name != "drive-efidisk0" || s.h.Devices[1].Bitmap != "reuse" || s.h.Devices[1].BitmapName != "snapshot-access:restic-pve" {
		t.Fatalf("header %+v", s.h)
	}
	if s.configs["qemu-server.conf"] != jobSpec(true).Config || s.configs["qemu-server.fw"] != "[OPTIONS]\nenable: 1\n" {
		t.Fatalf("configs %q", s.configs)
	}
	m := s.maps["drive-scsi0"]
	var dirty []uint64
	for i := uint64(0); i < 16; i++ {
		if m.Dirty(i) {
			dirty = append(dirty, i)
		}
	}
	if fmt.Sprint(dirty) != "[0 4 5]" || !s.maps["drive-efidisk0"].Recorded || s.maps["drive-efidisk0"].Dirty(0) {
		t.Fatalf("maps: scsi0 %v efidisk0 %+v", dirty, s.maps["drive-efidisk0"])
	}
	if len(s.pieces) != 0 || *s.end != (changes.End{}) {
		t.Fatalf("a probe sent pieces: %v %+v", s.pieces, s.end)
	}
	// nothing read from the disks: block status only
	if log := f.nbdLog(); strings.Contains(log, " read ") || strings.Contains(log, " trim ") || !strings.Contains(log, "drive-scsi0 status 0 67108864") {
		t.Fatalf("nbd commands:\n%s", log)
	}
	for _, want := range []string{"INFO: starting new backup job: vzdump 100 --storage restic-pve", "job ok, guest ok",
		"INFO: restic-pve: drive-scsi0: record reuse, 3 of 16 pieces of 4 MiB changed",
		"log info restic-pve: probe: nothing read or stored (drive-efidisk0 record reuse, 0 of 1 pieces changed; drive-scsi0 record reuse, 3 of 16 pieces changed)",
		"call backup_cleanup success"} {
		if !strings.Contains(s.stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, s.stderr)
		}
	}
	argv, _ := f.read("vzdump.argv")
	if argv != "100\n--storage\nrestic-pve\n--mode\nsnapshot\n--bwlimit\n40960\n--ionice\n7\n--performance\nmax-workers=1\n--fleecing\nenabled=1,storage=fast-ssd\n"+
		"--protected\n0\n--prune-backups\nkeep-all=1\n--remove\n0\n" {
		t.Fatalf("vzdump argv %q", argv)
	}
	f.clean("probe")
}

func TestChangesUseReadsOnlyTheMarkedPieces(t *testing.T) {
	f := newFixture(t)
	f.enable()
	f.providerVzdump(jobSpec(true, [2]uint64{piece, piece}, [2]uint64{10 * piece, 3 * piece}))
	s := f.job("changes 100 use", false, ackLine(changes.VerbOK))
	if s.code != 0 || s.err != io.EOF {
		t.Fatalf("%d %v\n%s", s.code, s.err, s.stderr)
	}
	if fmt.Sprint(s.pieces["drive-scsi0"]) != fmt.Sprint([]uint64{piece, 10 * piece, 11 * piece, 12 * piece}) || len(s.pieces["drive-efidisk0"]) != 0 {
		t.Fatalf("pieces %v", s.pieces)
	}
	disk := nbdtest.Pattern("drive-scsi0", 64<<20)
	for _, off := range s.pieces["drive-scsi0"] {
		if !bytes.Equal(s.data[fmt.Sprintf("drive-scsi0 %d", off)], disk[off:off+piece]) {
			t.Fatalf("piece at %d differs from the export", off)
		}
	}
	if *s.end != (changes.End{PiecesSent: 4, BytesSent: 4 * piece}) {
		t.Fatalf("end %+v", s.end)
	}
	// every piece read was discarded afterwards, nothing else was read
	log := f.nbdLog()
	for _, off := range s.pieces["drive-scsi0"] {
		if !strings.Contains(log, fmt.Sprintf("drive-scsi0 read %d %d\ndrive-scsi0 trim %d %d\n", off, piece, off, piece)) {
			t.Errorf("piece %d not read then trimmed:\n%s", off, log)
		}
	}
	if strings.Count(log, " read ") != 4 {
		t.Fatalf("reads:\n%s", log)
	}
	if !strings.Contains(s.stderr, "result archive-size 16777216") || !strings.Contains(s.stderr, "job ok, guest ok") ||
		!strings.Contains(s.stderr, "log info restic-pve: stored: confirmed by the backup machine as snapshot "+strings.Repeat("ab", 32)+" (") {
		t.Fatalf("stderr:\n%s", s.stderr)
	}
	f.clean("use")
}

// The header's prev is the token of the guest's last job that ended with a success ack (probe,
// arm or ok), "" before the first; a job that failed leaves it as it was.
func TestChangesPrev(t *testing.T) {
	f := newFixture(t)
	f.enable()
	f.providerVzdump(jobSpec(true, [2]uint64{0, piece}))
	prevIs := func(s stream, want string) {
		t.Helper()
		if s.h == nil || s.h.Prev == nil || *s.h.Prev != want {
			t.Fatalf("prev %v, want %q\n%s", s.h, want, s.stderr)
		}
	}
	s1 := f.job("changes 100 probe", false, ackLine(changes.VerbProbe))
	prevIs(s1, "")
	s2 := f.job("changes 100 use", false, ackLine(changes.VerbFail))
	prevIs(s2, s1.h.Token)
	if s2.code == 0 {
		t.Fatal("a failed ack ended the job well")
	}
	s3 := f.job("changes 100 new", false, ackLine(changes.VerbArm))
	prevIs(s3, s1.h.Token)
	s4 := f.job("changes 100 use", false, ackLine(changes.VerbOK))
	prevIs(s4, s3.h.Token)
	s5 := f.job("changes 100 probe", false, ackLine(changes.VerbProbe))
	prevIs(s5, s4.h.Token)
	p := f.root(filepath.Join(provider.RunDir, "job-100"))
	if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("last-job file: %v %v", st, err)
	}
	os.WriteFile(p, []byte("not a token\n"), 0o600)
	prevIs(f.job("changes 100 probe", false, ackLine(changes.VerbProbe)), "")
	f.clean("prev")
}

// Mode new, and mode use with a disk Proxmox has no record for: no map, no read, ack arm.
func TestChangesNewAndNoRecord(t *testing.T) {
	for _, c := range []struct {
		req    string
		record bool
	}{{"changes 100 new", true}, {"changes 100 use", false}} {
		f := newFixture(t)
		f.enable()
		f.providerVzdump(jobSpec(c.record, [2]uint64{0, piece}))
		s := f.job(c.req, false, ackLine(changes.VerbArm))
		if s.code != 0 || s.err != io.EOF {
			t.Fatalf("%s: %d %v\n%s", c.req, s.code, s.err, s.stderr)
		}
		for name, m := range s.maps {
			if m.Recorded {
				t.Errorf("%s: %s has a map", c.req, name)
			}
		}
		if s.h.Devices[1].Bitmap != "new" || len(s.pieces) != 0 || strings.Contains(f.nbdLog(), " read ") {
			t.Fatalf("%s: %+v %v", c.req, s.h.Devices, s.pieces)
		}
		if !strings.Contains(s.stderr, "result incremental drive-scsi0 "+map[bool]string{true: "new", false: "use"}[c.req == "changes 100 new"]) ||
			!strings.Contains(s.stderr, "log info restic-pve: armed: change record started, nothing stored (") {
			t.Fatalf("%s: incremental:\n%s", c.req, s.stderr)
		}
		f.clean(c.req)
	}
}

// Every ack that is not the expected success: Proxmox hears failure (backup_cleanup false),
// vzdump fails, the host is clean.
func TestChangesWithoutAValidAckFails(t *testing.T) {
	for label, ack := range map[string]func(*changes.Header) string{
		"no ack (EOF)": func(*changes.Header) string { return "" },
		"fail":         ackLine(changes.VerbFail),
		"wrong token": func(*changes.Header) string {
			return "ack " + strings.Repeat("0", 32) + " ok " + strings.Repeat("ab", 32) + "\n"
		},
		"wrong verb":     ackLine(changes.VerbArm),
		"malformed":      func(*changes.Header) string { return "ok\n" },
		"half a line":    func(h *changes.Header) string { return "ack " + h.Token },
		"probe for use":  ackLine(changes.VerbProbe),
		"ok, bad digest": func(h *changes.Header) string { return "ack " + h.Token + " ok xyz\n" },
	} {
		f := newFixture(t)
		f.enable()
		f.providerVzdump(jobSpec(true, [2]uint64{0, piece}))
		s := f.job("changes 100 use", false, ack)
		if s.code != 2 || !strings.Contains(s.stderr, "guest failed: restic-pve: the backup machine did not confirm the backup") ||
			!strings.Contains(s.stderr, "call backup_cleanup failure") || strings.Contains(s.stderr, "call backup_cleanup success") {
			t.Errorf("%s: %d\n%s", label, s.code, s.stderr)
		}
		f.clean(label)
	}
}

func TestChangesConnectionLostMidStream(t *testing.T) {
	f := newFixture(t)
	f.enable()
	f.providerVzdump(jobSpec(true, [2]uint64{0, 8 * piece}))
	s := f.job("changes 100 use", true, nil)
	if s.code == 0 || !strings.Contains(s.stderr, "guest failed: restic-pve: ") || strings.Contains(s.stderr, "call backup_cleanup success") {
		t.Fatalf("%d\n%s", s.code, s.stderr)
	}
	f.clean("cut")
}

func TestChangesVzdumpFails(t *testing.T) {
	f := newFixture(t)
	f.enable()
	f.providerVzdump(jobSpec(true))
	f.set("vzdump.rc", "5")
	s := f.job("changes 100 probe", false, ackLine(changes.VerbProbe))
	if s.code != 5 || s.h != nil || !strings.Contains(s.stderr, "ERROR: Backup of VM 100 failed - fake failure") {
		t.Fatalf("%d %+v\n%s", s.code, s.h, s.stderr)
	}
	f.clean("vzdump failed")
}

func TestChangesMisalignedRecordIsExit3(t *testing.T) {
	f := newFixture(t)
	f.enable()
	f.providerVzdump(jobSpec(true, [2]uint64{piece, 1 << 20}))
	s := f.job("changes 100 use", false, ackLine(changes.VerbOK))
	if s.code != 3 || !strings.Contains(s.stderr, "restic-pve: error: drive-scsi0: change record not on 4 MiB boundaries") ||
		strings.Contains(f.nbdLog(), " read ") {
		t.Fatalf("%d\n%s", s.code, s.stderr)
	}
	f.clean("misaligned")
}

// The checks before vzdump starts: 79 with the fix, nothing started, the host unchanged.
func TestChangesNotReady(t *testing.T) {
	cases := []struct {
		label, want string
		setup       func(f *fixture)
	}{
		{"feature off", "plug-in not ready: changed reads are off on this host; run restic-pve install on the Proxmox host and answer yes to changed reads",
			func(f *fixture) { os.Remove(f.root(provider.PluginPath)); os.Remove(f.root(provider.StorageConfig)) }},
		{"foreign file", "plug-in not ready: " + provider.PluginPath + " was not written by restic-pve; move it away",
			func(f *fixture) { f.writeFile(f.root(provider.PluginPath), "package X;\n1;\n", 0o644) }},
		{"no storage entry", "plug-in not ready: the storage entry restic-pve is missing",
			func(f *fixture) { f.writeFile(f.root(provider.StorageConfig), "dir: local\n\tpath /x\n", 0o644) }},
		{"not loaded", "plug-in not ready: Proxmox did not load the plug-in (pvesm status does not show restic-pve active)",
			func(f *fixture) { f.set("pvesm-restic-pve.txt", pvesm(0, "restic-pve", "inactive")) }},
		{"pvesm fails", "plug-in not ready: Proxmox did not load the plug-in", func(f *fixture) { f.rm("pvesm-restic-pve.txt") }},
		{"cluster", "plug-in not ready: this host is part of a Proxmox cluster; changed reads work on single hosts for now; run restic-pve install on the Proxmox host and answer no to changed reads",
			func(f *fixture) { f.writeFile(f.root(provider.CorosyncConfig), "totem {}\n", 0o640) }},
	}
	for _, c := range cases {
		f := newFixture(t)
		f.enable()
		c.setup(f)
		r := f.req("changes 100 probe")
		if r.code != 79 || r.stdout != "" || !strings.Contains(r.stderr, "restic-pve: error: "+c.want) {
			t.Errorf("%s: %+v", c.label, r)
		}
		f.noVzdump()
		if _, err := os.Lstat(f.root(provider.RunDir)); err == nil {
			t.Errorf("%s: job folder made", c.label)
		}
	}
	// busy and no space come first, as for dump
	f := newFixture(t)
	f.enable()
	fh := lockFile(t, f)
	if r := f.req("changes 100 use"); r.code != 75 || !strings.Contains(r.stderr, "busy") {
		t.Fatalf("lock: %+v", r)
	}
	fh.Close()
	f.set("pvesm-fast-ssd.txt", pvesm(31*1048576, "fast-ssd", "active"))
	if r := f.req("changes 100 use"); r.code != 73 {
		t.Fatalf("space: %+v", r)
	}
	f.noVzdump()
}

// A process that is not below the started vzdump cannot use the job socket; a second job
// while one runs is busy.
func TestChangesRefusesForeignConnectionsAndASecondJob(t *testing.T) {
	f := newFixture(t)
	f.enable()
	f.providerVzdump(jobSpec(true, [2]uint64{0, piece}))
	f.set("vzdump.sleep", "2")
	res := make(chan stream, 1)
	go func() { res <- f.job("changes 100 probe", false, ackLine(changes.VerbProbe)) }()
	sock := f.root(provider.RunDir + "/provider.sock")
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no socket")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st, _ := os.Lstat(f.root(provider.RunDir)); st.Mode().Perm() != 0o700 {
		t.Fatalf("job folder mode %v", st.Mode())
	}
	if st, _ := os.Lstat(sock); st.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v", st.Mode())
	}
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte(`{"method":"job_init","args":{"protocol":1,"start_time":0}}` + "\n"))
	c.SetDeadline(time.Now().Add(5 * time.Second))
	b := make([]byte, 200)
	n, _ := c.Read(b)
	c.Close()
	if string(b[:n]) != `{"ok":false,"message":"not a job started by restic-pve"}`+"\n" {
		t.Fatalf("foreign connection answered %q", b[:n])
	}
	if r := f.req("changes 100 use"); r.code != 75 {
		t.Fatalf("second job: %+v", r)
	}
	if r := f.req("dump 100"); r.code != 75 {
		t.Fatalf("dump during a job: %+v", r)
	}
	s := <-res
	if s.code != 0 || !strings.Contains(s.stderr, "WARN: restic-pve: refused a connection to the job socket: peer process") {
		t.Fatalf("%d\n%s", s.code, s.stderr)
	}
	f.clean("foreign connection")
}

// A host that rebooted or a request that was killed leaves a socket; the next job removes it.
func TestChangesAfterACrash(t *testing.T) {
	f := newFixture(t)
	f.enable()
	f.providerVzdump(jobSpec(true))
	os.MkdirAll(f.root(provider.RunDir), 0o755)
	l, err := net.Listen("unix", f.root(provider.RunDir+"/provider.sock"))
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	s := f.job("changes 100 probe", false, ackLine(changes.VerbProbe))
	if s.code != 0 {
		t.Fatalf("%d\n%s", s.code, s.stderr)
	}
	f.clean("stale socket")
}

// SIGTERM (the ssh session ends) while the job waits for the ack: vzdump is stopped, Proxmox
// hears failure, the host is clean.
func TestChangesSignal(t *testing.T) {
	f := newFixture(t)
	f.enable()
	f.providerVzdump(jobSpec(true))
	c := f.cmd("changes 100 probe")
	c.WaitDelay = 20 * time.Second
	stdin, _ := c.StdinPipe()
	stdout, _ := c.StdoutPipe()
	var errb bytes.Buffer
	c.Stderr = &errb
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	dec := changes.NewDecoder(stdout)
	for {
		fr, err := dec.Next()
		if err != nil {
			t.Fatal(err)
		}
		if fr.End != nil {
			break
		}
	}
	c.Process.Signal(syscall.SIGTERM)
	io.Copy(io.Discard, stdout)
	c.Wait()
	stdin.Close()
	if c.ProcessState.ExitCode() != 143 || !strings.Contains(errb.String(), "INFO: restic-pve: no confirmed backup: changes: no valid ack: the request got terminated") ||
		strings.Contains(errb.String(), "call backup_cleanup success") {
		t.Fatalf("%d\n%s", c.ProcessState.ExitCode(), errb.String())
	}
	f.clean("signal")
}

// The read limit: BWLIMIT_KIB paces the reads (one piece may go ahead).
func TestChangesReadLimit(t *testing.T) {
	f := newFixture(t)
	f.conf(strings.Replace(testConf, "BWLIMIT_KIB=40960", "BWLIMIT_KIB=16384", 1))
	f.enable()
	f.providerVzdump(jobSpec(true, [2]uint64{0, 9 * piece}))
	start := time.Now()
	s := f.job("changes 100 use", false, ackLine(changes.VerbOK))
	if s.code != 0 || len(s.pieces["drive-scsi0"]) != 9 {
		t.Fatalf("%d %v\n%s", s.code, s.pieces, s.stderr)
	}
	// 36 MiB at 16 MiB/s with 4 MiB ahead: at least 2 s
	if d := time.Since(start); d < 2*time.Second {
		t.Fatalf("read limit not kept: %v", d)
	}
}

func TestBucket(t *testing.T) {
	now := time.Unix(0, 0)
	var slept time.Duration
	b := &bucket{rate: 4 << 20, now: func() time.Time { return now }, sleep: func(d time.Duration) { slept += d; now = now.Add(d) }}
	for i := 0; i < 5; i++ {
		b.wait(piece)
	}
	// 20 MiB at 4 MiB/s, the first piece free: 4 s
	if slept != 4*time.Second {
		t.Fatalf("slept %v", slept)
	}
	off := newBucket(0)
	off.wait(1 << 30) // no limit: returns at once
}

// The plug-in follows this binary: another version on disk is rewritten under the lock before
// vzdump starts, a current one is left alone; host-sync waits for a running job.
func TestPluginSyncOnUpdateAndBeforeEveryJob(t *testing.T) {
	f := newFixture(t)
	f.enable()
	f.providerVzdump(jobSpec(true))
	old := provider.Marker + "\n# the plug-in of an older release\n1;\n"
	plugin := f.root(provider.PluginPath)
	hostSync := func() result {
		exe, _ := os.Executable()
		c := exec.Command(exe)
		c.Env = []string{"RESTICPVE_TEST_DIR=" + f.dir, "RESTICPVE_TEST_HOSTSYNC=1"}
		return f.run(c)
	}
	// same version: untouched
	before, _ := os.Stat(plugin)
	if r := hostSync(); r.code != 0 || r.stdout != "unchanged "+provider.PluginPath+"\n" {
		t.Fatalf("current: %+v", r)
	}
	if after, _ := os.Stat(plugin); !os.SameFile(before, after) {
		t.Fatal("current plug-in rewritten")
	}
	// old plug-in, lock free: rewritten
	f.writeFile(plugin, old, 0o644)
	if r := hostSync(); r.code != 0 || r.stdout != "written   "+provider.PluginPath+"\n" {
		t.Fatalf("old: %+v", r)
	}
	if b, _ := os.ReadFile(plugin); !bytes.Equal(b, provider.Plugin) {
		t.Fatal("not rewritten")
	}
	// a job runs: host-sync changes nothing; the job ends; the next job syncs before vzdump
	f.writeFile(plugin, old, 0o644)
	fh := lockFile(t, f)
	if r := hostSync(); r.code != 0 || !strings.Contains(r.stdout, "a backup is running; the plug-in is brought up to date when the next backup starts") {
		t.Fatalf("busy: %+v", r)
	}
	if b, _ := os.ReadFile(plugin); string(b) != old {
		t.Fatal("plug-in changed under a running backup")
	}
	fh.Close()
	s := f.job("changes 100 probe", false, ackLine(changes.VerbProbe))
	if s.code != 0 || !strings.Contains(s.stderr, "INFO: restic-pve: written "+provider.PluginPath) {
		t.Fatalf("%d\n%s", s.code, s.stderr)
	}
	if strings.Index(s.stderr, "written "+provider.PluginPath) > strings.Index(s.stderr, "starting new backup job") {
		t.Fatal("synced after vzdump started")
	}
	f.clean("synced")
	// feature off: nothing to do, nothing created
	os.Remove(plugin)
	if r := hostSync(); r.code != 0 || !strings.Contains(r.stdout, "changed reads are off on this host") {
		t.Fatalf("off: %+v", r)
	}
	if _, err := os.Stat(plugin); err == nil {
		t.Fatal("host-sync created the plug-in")
	}
	// foreign: refused, untouched
	f.writeFile(plugin, "package X;\n1;\n", 0o644)
	if r := hostSync(); r.code != 1 || !strings.Contains(r.stderr, "was not written by restic-pve") {
		t.Fatalf("foreign: %+v", r)
	}
}

// info reports the state of changed reads; without the feature it asks pvesm nothing more.
func TestInfoProvider(t *testing.T) {
	f := newFixture(t)
	type prov struct {
		Plugin                   string
		Storage, Active, Cluster bool
		API                      string
	}
	get := func() prov {
		var p prov
		b, _ := json.Marshal(f.info("info 100")["provider"])
		json.Unmarshal(b, &p)
		return p
	}
	if p := get(); p != (prov{"absent", false, false, false, "unchecked"}) {
		t.Fatalf("off: %+v", p)
	}
	if log, _ := f.read("pvesm.log"); strings.Contains(log, "restic-pve") {
		t.Fatalf("asked pvesm about the storage on a host without it: %q", log)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "perl.ran")); err == nil {
		t.Fatal("ran Perl on a host without the feature")
	}
	f.enable()
	if p := get(); p != (prov{"current", true, true, false, "ok"}) {
		t.Fatalf("on: %+v", p)
	}
	f.set("perl-api.txt", "api 18 8 base 1\n")
	f.writeFile(f.root(provider.CorosyncConfig), "", 0o640)
	if p := get(); p != (prov{"current", true, true, true, "newer"}) {
		t.Fatalf("newer Proxmox, cluster: %+v", p)
	}
	f.set("perl-api.txt", "api 27 8 base 1\n")
	if p := get(); p.API != "unsupported" {
		t.Fatalf("too new: %+v", p)
	}
	f.set("perl-api.txt", "api 17 8 base 1\n")
	os.Remove(f.root(provider.CorosyncConfig))
	f.writeFile(f.root(provider.PluginPath), provider.Marker+"\nold\n", 0o644)
	f.set("pvesm-restic-pve.txt", pvesm(0, "restic-pve", "inactive"))
	if p := get(); p != (prov{"different", true, false, false, "ok"}) {
		t.Fatalf("old plug-in, not loaded: %+v", p)
	}
	f.writeFile(f.root(provider.PluginPath), "package X;\n", 0o644)
	f.writeFile(f.root(provider.StorageConfig), "dir: local\n", 0o644)
	if p := get(); p != (prov{"foreign", false, false, false, "unchecked"}) {
		t.Fatalf("foreign: %+v", p)
	}
	f.noVzdump()
}

// A backup machine that stops reading: the job ends after the bound, Proxmox hears failure,
// the host is clean.
func TestChangesStalledReader(t *testing.T) {
	f := newFixture(t)
	f.enable()
	f.providerVzdump(jobSpec(true, [2]uint64{0, 16 * piece}))
	c := f.cmd("changes 100 use")
	c.Env = append(c.Env, "RESTICPVE_TEST_STALL=1s")
	c.WaitDelay = 20 * time.Second
	stdin, _ := c.StdinPipe()
	stdout, _ := c.StdoutPipe()
	var errb bytes.Buffer
	c.Stderr = &errb
	start := time.Now()
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := changes.NewDecoder(stdout).Next(); err != nil { // the header, then nothing more is read
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the job did not end")
	}
	stdin.Close()
	if c.ProcessState.ExitCode() == 0 || !strings.Contains(errb.String(), "the backup machine read nothing of the stream for 1s") ||
		strings.Contains(errb.String(), "call backup_cleanup success") || time.Since(start) > 20*time.Second {
		t.Fatalf("%d %v\n%s", c.ProcessState.ExitCode(), time.Since(start), errb.String())
	}
	f.clean("stalled reader")
}

// An ack that comes after the limit is too late.
func TestChangesLateAck(t *testing.T) {
	f := newFixture(t)
	f.enable()
	f.providerVzdump(jobSpec(true, [2]uint64{0, piece}))
	c := f.cmd("changes 100 probe")
	c.Env = append(c.Env, "RESTICPVE_TEST_ACK=1s")
	c.WaitDelay = 20 * time.Second
	stdin, _ := c.StdinPipe()
	stdout, _ := c.StdoutPipe()
	var errb bytes.Buffer
	c.Stderr = &errb
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	dec := changes.NewDecoder(stdout)
	var h *changes.Header
	for {
		fr, err := dec.Next()
		if err != nil {
			t.Fatal(err)
		}
		if fr.Header != nil {
			h = fr.Header
		}
		if fr.End != nil {
			break
		}
	}
	time.Sleep(2 * time.Second)
	io.WriteString(stdin, ackLine(changes.VerbProbe)(h))
	stdin.Close()
	io.Copy(io.Discard, stdout)
	c.Wait()
	if c.ProcessState.ExitCode() == 0 || !strings.Contains(errb.String(), "none within 1s after the end of the stream") ||
		strings.Contains(errb.String(), "call backup_cleanup success") {
		t.Fatalf("%d\n%s", c.ProcessState.ExitCode(), errb.String())
	}
	f.clean("late ack")
}

// A disk that was never queried, even of size 0, is refused; so is a name off the pattern.
func TestBackupVMRefusesUnqueriedDisks(t *testing.T) {
	out, _ := os.CreateTemp(t.TempDir(), "out")
	defer out.Close()
	j := &job{e: &Env{Stdout: out, Stderr: out, AckTimeout: time.Second}, vmid: 100, mode: "use",
		volumes: map[string]uint64{"drive-scsi0": 64 << 20}, out: stallWriter(out, time.Second)}
	cfg := "bmFtZTogeA=="
	for _, vols := range []map[string]volume{
		{"drive-sata0": {Size: 0, BitmapMode: "new"}},
		{"drive-scsi0": {Size: 1, BitmapMode: "new"}},
		{"../x": {Size: 0, BitmapMode: "new"}},
	} {
		if _, _, err := j.backupVM(t.Context(), 100, cfg, nil, vols, 0); err == nil || !strings.Contains(err.Error(), "differs from the query") {
			t.Errorf("%v: %v", vols, err)
		}
	}
	if st, _ := out.Stat(); st.Size() != 0 {
		t.Fatal("wrote frames for a refused disk set")
	}
	// the descriptor survives a collection: the writer keeps its file
	runtime.GC()
	runtime.GC()
	if _, err := out.Stat(); err != nil {
		t.Fatalf("descriptor closed behind our back: %v", err)
	}
}

// host-sync on a cluster and with a newer Proxmox says so.
func TestHostSyncNotes(t *testing.T) {
	f := newFixture(t)
	f.enable()
	f.writeFile(f.root(provider.CorosyncConfig), "", 0o640)
	f.set("perl-api.txt", "api 18 8 base 1\n")
	exe, _ := os.Executable()
	c := exec.Command(exe)
	c.Env = []string{"RESTICPVE_TEST_DIR=" + f.dir, "RESTICPVE_TEST_HOSTSYNC=1"}
	r := f.run(c)
	if r.code != 0 || !strings.Contains(r.stdout, "part of a Proxmox cluster, where changed reads are refused") ||
		!strings.Contains(r.stdout, "this Proxmox (storage API 18) is newer than the plug-in was checked against (17)") {
		t.Fatalf("%+v", r)
	}
}
