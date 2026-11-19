package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/norandom/restic-pve/internal/stream/reorder"
	"github.com/norandom/restic-pve/internal/stream/rvs"
	"github.com/norandom/restic-pve/internal/stream/verify"
	"github.com/norandom/restic-pve/internal/stream/vma"
	"github.com/norandom/restic-pve/internal/stream/vma/vmatest"
)

// --- fixtures ---

func testGuest() *vmatest.Builder {
	devs := []vmatest.Device{
		vmatest.EFIDisk(1, 256*1024, 21),
		vmatest.DataDisk(2, "scsi0", 12<<20+1000, 22), // short final cluster
		vmatest.TPMState(3, 64*1024, 23),
	}
	return &vmatest.Builder{
		UUID:    [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		CTime:   1760000000,
		Devices: devs,
		Configs: []vma.ConfigBlob{vmatest.ExampleConfig()},
		Order:   vmatest.InterleavedOrder(devs),
	}
}

func writeVMA(t *testing.T, b *vmatest.Builder) string {
	t.Helper()
	res, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "guest.vma")
	if err := os.WriteFile(p, res.Stream, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

type backupRun struct {
	code     exitCode
	stdout   []byte
	stderr   string
	manifest string
}

func backupArgs(manifest string, extra ...string) []string {
	return append([]string{"backup", "--vmid", "100", "--manifest-out", manifest,
		"--ram-mib", "16", "--progress-interval", "1ns"}, extra...)
}

func runBackup(t *testing.T, stdout io.Writer, extra ...string) backupRun {
	t.Helper()
	dir := t.TempDir()
	m := filepath.Join(dir, "vm-100.manifest.json")
	var out bytes.Buffer
	var w io.Writer = &out
	if stdout != nil {
		w = stdout
	}
	var errb bytes.Buffer
	code := run(backupArgs(m, extra...), strings.NewReader(""), w, &errb)
	return backupRun{code: code, stdout: out.Bytes(), stderr: errb.String(), manifest: m}
}

// jsonLines parses every stderr line that starts with '{' and checks that
// it is a JSON object with a type.
func jsonLines(t *testing.T, stderr string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSuffix(stderr, "\n"), "\n") {
		if !strings.HasPrefix(l, "{") {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("stderr JSON line does not parse: %q (%v)", l, err)
		}
		if _, ok := m["type"].(string); !ok {
			t.Fatalf("JSON line without type: %q", l)
		}
		out = append(out, m)
	}
	return out
}

func ofType(lines []map[string]any, typ string) []map[string]any {
	var out []map[string]any
	for _, l := range lines {
		if l["type"] == typ {
			out = append(out, l)
		}
	}
	return out
}

// readRVS reads a complete RVS1 stream and rebuilds each device's content
// (patches overlaid on data frames) to compare with the builder.
func readRVS(t *testing.T, b []byte) (*rvs.Trailer, map[uint8][]byte) {
	t.Helper()
	r, err := rvs.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("rvs header: %v", err)
	}
	devs := map[uint8][]byte{}
	for _, d := range r.Header().Devices {
		devs[d.ID] = make([]byte, d.Size)
	}
	patched := map[[2]uint64]bool{}
	for {
		h, p, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("rvs frame: %v", err)
		}
		data, err := io.ReadAll(p)
		if err != nil {
			t.Fatal(err)
		}
		switch h.Type {
		case rvs.TypePatch:
			copy(devs[uint8(h.Dev)][h.Offset:], data)
			for o := uint64(0); o < uint64(len(data)); o += rvs.ClusterSize {
				patched[[2]uint64{uint64(h.Dev), h.Offset + o}] = true
			}
		case rvs.TypeData:
			for o := uint64(0); o < uint64(len(data)); o += rvs.ClusterSize {
				if !patched[[2]uint64{uint64(h.Dev), h.Offset + o}] {
					copy(devs[uint8(h.Dev)][h.Offset+o:], data[o:min(o+rvs.ClusterSize, uint64(len(data)))])
				}
			}
		}
	}
	tr, err := r.Trailer()
	if err != nil {
		t.Fatalf("trailer: %v", err)
	}
	return tr, devs
}

func expectedDigest(content []byte) string {
	d := verify.NewDigest(uint64(len(content)))
	for i := uint64(0); i*vma.ClusterSize < uint64(len(content)); i++ {
		lo := i * vma.ClusterSize
		d.Cluster(uint32(i), content[lo:min(lo+vma.ClusterSize, uint64(len(content)))])
	}
	return verify.Hex(d.Sum())
}

func assertNoManifest(t *testing.T, r backupRun) {
	t.Helper()
	if _, err := os.Stat(r.manifest); !os.IsNotExist(err) {
		t.Fatalf("manifest written on failure (stat: %v)", err)
	}
	ents, _ := os.ReadDir(filepath.Dir(r.manifest))
	if len(ents) != 0 {
		t.Fatalf("leftover files next to the manifest: %v", ents[0].Name())
	}
}

// assertIncomplete checks that stdout is not a complete RVS1 stream.
func assertIncomplete(t *testing.T, b []byte) {
	t.Helper()
	r, err := rvs.NewReader(bytes.NewReader(b))
	if err != nil {
		return
	}
	for {
		_, p, err := r.Next()
		if err == io.EOF {
			t.Fatal("a complete RVS1 stream reached stdout on failure")
		}
		if err != nil {
			return
		}
		io.Copy(io.Discard, p)
	}
}

func assertResult(t *testing.T, r backupRun, code exitCode, transient bool) resultLine {
	t.Helper()
	if r.code != code {
		t.Fatalf("exit = %d, want %d; stderr:\n%s", r.code, code, r.stderr)
	}
	res := lastResult(t, r.stderr)
	if res.Code != int(code) || res.Transient != transient {
		t.Fatalf("result %+v, want code %d transient %v", res, code, transient)
	}
	return res
}

// --- --input pipeline ---

func TestBackupInputProducesRVS1AndManifest(t *testing.T) {
	b := testGuest()
	in := writeVMA(t, b)
	r := runBackup(t, nil, "--input", in)
	res := assertResult(t, r, exitOK, false)
	if res.Status != "ok" {
		t.Fatalf("result %+v", res)
	}
	tr, devs := readRVS(t, r.stdout)
	for _, d := range b.Devices {
		if !bytes.Equal(devs[d.ID], b.Contents(d.ID)) {
			t.Fatalf("device %d content differs after RVS1 round trip", d.ID)
		}
	}
	if len(tr.Devices) != 3 {
		t.Fatalf("trailer devices %+v", tr.Devices)
	}
	for _, td := range tr.Devices {
		if want := expectedDigest(b.Contents(td.ID)); td.SHA256CT != want {
			t.Fatalf("device %d sha256ct %s, want %s", td.ID, td.SHA256CT, want)
		}
	}
	if tr.VMACTime != 1760000000 || tr.VMAUUID != "01020304-0506-0708-090a-0b0c0d0e0f10" {
		t.Fatalf("trailer uuid/ctime %q %d", tr.VMAUUID, tr.VMACTime)
	}
	if tr.Consistency != "crash" || !strings.HasPrefix(tr.Tool, "vmastream ") {
		t.Fatalf("trailer consistency %q tool %q", tr.Consistency, tr.Tool)
	}

	m, err := verify.ReadManifestFile(r.manifest)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if err := verify.Compare(tr, m); err != nil {
		t.Fatalf("manifest does not match the trailer: %v", err)
	}
	if m.VMID != 100 || m.PVEName != "example" || m.OSType != "l26" || m.Consistency != "crash" ||
		m.ConsistencyDetail != "agent status unknown" || m.VMAUUID != tr.VMAUUID || m.Started == "" || m.Finished == "" {
		t.Fatalf("manifest fields %+v", m)
	}
	if len(m.Configs) != 1 || m.Configs[0].Name != "qemu-server.conf" {
		t.Fatalf("manifest configs %+v", m.Configs)
	}

	result := lastResult(t, r.stderr)
	if result.BufferRAMPeak == nil || *result.BufferRAMPeak != tr.BufferRAMPeak ||
		result.PatchCount == nil || *result.PatchCount != tr.PatchCount {
		t.Fatalf("result stats %+v differ from trailer %+v", result, tr)
	}
	lines := jsonLines(t, r.stderr)
	prog := ofType(lines, "progress")
	if len(prog) == 0 {
		t.Fatal("no progress lines")
	}
	for _, p := range prog {
		for _, k := range []string{"dev", "bytes_done", "total_bytes", "percent", "ram_bytes", "patches"} {
			if _, ok := p[k]; !ok {
				t.Fatalf("progress line lacks %q: %v", k, p)
			}
		}
		if p["ram_bytes"].(float64) > float64(16<<20) {
			t.Fatalf("ram_bytes above the cap: %v", p)
		}
	}
	last := prog[len(prog)-1]
	if last["percent"].(float64) != 100 || last["bytes_done"] != last["total_bytes"] {
		t.Fatalf("final progress %v", last)
	}
	cons := ofType(lines, "consistency")
	if len(cons) != 1 || cons[0]["value"] != "crash" || cons[0]["detail"] != "agent status unknown" {
		t.Fatalf("consistency lines %v", cons)
	}
	if lines[len(lines)-1]["type"] != "result" {
		t.Fatal("result is not the last JSON line")
	}
}

func TestBackupOverflowStillCompletes(t *testing.T) {
	b := testGuest()
	// far jumps: the back half of the data disk arrives first
	seq := vmatest.SequentialOrder(b.Devices)
	b.Order = append(append([]vmatest.ClusterRef{}, seq[len(seq)/2:]...), seq[:len(seq)/2]...)
	in := writeVMA(t, b)
	r := runBackup(t, nil, "--input", in, "--ram-mib", "1")
	assertResult(t, r, exitOK, false)
	tr, devs := readRVS(t, r.stdout)
	for _, d := range b.Devices {
		if !bytes.Equal(devs[d.ID], b.Contents(d.ID)) {
			t.Fatalf("device %d differs", d.ID)
		}
	}
	if tr.PatchCount == 0 {
		t.Fatal("expected patches with a 1 MiB RAM cap and no disk tier")
	}
	m, err := verify.ReadManifestFile(r.manifest)
	if err != nil || m.PatchCount != tr.PatchCount {
		t.Fatalf("manifest %+v (%v)", m, err)
	}
}

func TestBufferCapacityFailureExits7(t *testing.T) {
	fp := verify.NewFingerprint(reorder.Layout{Devices: []vma.DevInfo{{ID: 1, Size: vma.ClusterSize}}})
	f := bufferFailure(reorder.ErrCapacity, fp)
	code, transient, err := decide(f, nil)
	if code != exitLocalIO || transient || !errors.Is(err, reorder.ErrCapacity) {
		t.Fatalf("capacity failure = (%d, %v, %v), want exit 7 and ErrCapacity", code, transient, err)
	}
	want := "reorder: RAM cap exhausted by hash slots (raise --ram-mib or lower MAX_WORKERS in /etc/restic-pve.conf)"
	if err.Error() != want {
		t.Fatalf("message %q, want %q", err.Error(), want)
	}
}

func TestBackupTamperedExtentExits3(t *testing.T) {
	for name, kind := range map[string]vmatest.TamperKind{
		"extent md5":  vmatest.TamperExtentMD5,
		"extent uuid": vmatest.TamperExtentUUID,
		"reserved":    vmatest.TamperReserved,
		"header md5":  vmatest.TamperHeaderMD5,
		"duplicate":   vmatest.TamperDuplicateCluster,
	} {
		t.Run(name, func(t *testing.T) {
			b := testGuest()
			b.Tamper = vmatest.Tamper{Kind: kind, Extent: 3}
			r := runBackup(t, nil, "--input", writeVMA(t, b))
			assertResult(t, r, exitIntegrity, false)
			assertNoManifest(t, r)
			assertIncomplete(t, r.stdout)
		})
	}
}

func TestBackupVMStateExits4(t *testing.T) {
	b := testGuest()
	b.Devices = append(b.Devices, vmatest.VMStateDevice(5, 128*1024))
	b.Order = nil
	r := runBackup(t, nil, "--input", writeVMA(t, b))
	assertResult(t, r, exitForbiddenContent, false)
	assertNoManifest(t, r)
	if len(r.stdout) != 0 {
		t.Fatalf("stdout written before the forbidden device was rejected: %d bytes", len(r.stdout))
	}
}

func TestBackupTruncatedInputExits6(t *testing.T) {
	for name, kind := range map[string]vmatest.TamperKind{
		"cut inside extent": vmatest.TamperTruncate,
		"missing extent":    vmatest.TamperMissingCluster,
	} {
		t.Run(name, func(t *testing.T) {
			b := testGuest()
			b.Tamper = vmatest.Tamper{Kind: kind, Extent: 2}
			r := runBackup(t, nil, "--input", writeVMA(t, b))
			assertResult(t, r, exitSourceFailed, false)
			assertNoManifest(t, r)
			assertIncomplete(t, r.stdout)
		})
	}
}

func TestBackupMissingInputIsLocalIO(t *testing.T) {
	r := runBackup(t, nil, "--input", filepath.Join(t.TempDir(), "absent.vma"))
	assertResult(t, r, exitLocalIO, false)
	assertNoManifest(t, r)
}

type failWriter struct{ n int }

func (f *failWriter) Write(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, errors.New("broken pipe")
	}
	f.n--
	return len(p), nil
}

func TestBackupStdoutErrorExits7(t *testing.T) {
	r := runBackup(t, &failWriter{}, "--input", writeVMA(t, testGuest()))
	assertResult(t, r, exitLocalIO, false)
	assertNoManifest(t, r)
}

func TestBackupManifestWriteErrorExits7(t *testing.T) {
	in := writeVMA(t, testGuest())
	var out, errb bytes.Buffer
	m := filepath.Join(t.TempDir(), "absent-dir", "m.json")
	code := run(backupArgs(m, "--input", in), strings.NewReader(""), &out, &errb)
	assertResult(t, backupRun{code: code, stderr: errb.String()}, exitLocalIO, false)
}

func TestRAMCapMustCoverBitmapsAndMargin(t *testing.T) {
	l := reorder.Layout{Devices: []vma.DevInfo{{ID: 1, Name: "drive-scsi0", Size: 1 << 40}}} // 1 TiB: 2 MiB of bitmaps
	if err := checkRAMCap(4<<20, l); err != nil {
		t.Fatalf("4 MiB rejected: %v", err)
	}
	err := checkRAMCap(2<<20, l)
	var ue *usageError
	if !errors.As(err, &ue) || !strings.Contains(err.Error(), "--ram-mib") {
		t.Fatalf("2 MiB for 1 TiB: %v, want a usage error naming --ram-mib", err)
	}
	if err := checkRAMCap(reorder.BitmapBytes(l)+ramMarginBytes-1, l); err == nil {
		t.Fatal("cap below bitmaps + margin accepted")
	}
}

// --- ssh source (fake ssh on PATH) ---

func fakeSSH(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	b, err := os.ReadFile(filepath.Join("..", "source", "testdata", "fake-ssh.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ssh"), b, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	args := filepath.Join(dir, "args")
	t.Setenv("FAKE_SSH_ARGS", args)
	for _, k := range []string{"FAKE_SSH_LOG", "FAKE_SSH_STDOUT", "FAKE_SSH_SLEEP", "FAKE_SSH_EXIT"} {
		t.Setenv(k, "")
	}
	return args
}

func fixtureLog(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "source", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func sshArgs(extra ...string) []string {
	return append([]string{"--ssh-config", "ssh_config", "--ssh-host", "pve.example"}, extra...)
}

func TestBackupOverSSHApplicationConsistent(t *testing.T) {
	argsFile := fakeSSH(t)
	b := testGuest()
	t.Setenv("FAKE_SSH_STDOUT", writeVMA(t, b))
	t.Setenv("FAKE_SSH_LOG", fixtureLog(t, "application.log"))
	r := runBackup(t, nil, sshArgs()...)
	assertResult(t, r, exitOK, false)
	argv, _ := os.ReadFile(argsFile)
	if string(argv) != "-F\nssh_config\n-T\n-o\nBatchMode=yes\npve.example\ndump\n100\n" {
		t.Fatalf("ssh argv = %q", argv)
	}
	tr, devs := readRVS(t, r.stdout)
	for _, d := range b.Devices {
		if !bytes.Equal(devs[d.ID], b.Contents(d.ID)) {
			t.Fatalf("device %d differs", d.ID)
		}
	}
	if tr.Consistency != "application" {
		t.Fatalf("trailer consistency %q", tr.Consistency)
	}
	m, err := verify.ReadManifestFile(r.manifest)
	if err != nil || m.Consistency != "application" {
		t.Fatalf("manifest %+v (%v)", m, err)
	}
	lines := jsonLines(t, r.stderr)
	if c := ofType(lines, "consistency"); len(c) != 1 || c[0]["value"] != "application" {
		t.Fatalf("consistency lines %v", c)
	}
	if v := ofType(lines, "vzdump"); len(v) != 0 {
		t.Fatalf("info lines forwarded: %v", v)
	}
}

func TestBackupOverSSHStoppedGuest(t *testing.T) {
	fakeSSH(t)
	t.Setenv("FAKE_SSH_STDOUT", writeVMA(t, testGuest()))
	t.Setenv("FAKE_SSH_LOG", fixtureLog(t, "stopped.log"))
	r := runBackup(t, nil, sshArgs()...)
	assertResult(t, r, exitOK, false)
	tr, _ := readRVS(t, r.stdout)
	if tr.Consistency != "stopped" || len(tr.VzdumpWarnings) != 0 {
		t.Fatalf("trailer consistency %q, warnings %v", tr.Consistency, tr.VzdumpWarnings)
	}
	m, err := verify.ReadManifestFile(r.manifest)
	if err != nil || m.Consistency != "stopped" {
		t.Fatalf("manifest %+v (%v)", m, err)
	}
	lines := jsonLines(t, r.stderr)
	if c := ofType(lines, "consistency"); len(c) != 1 || c[0]["value"] != "stopped" {
		t.Fatalf("consistency lines %v", c)
	}
	if v := ofType(lines, "vzdump"); len(v) != 0 {
		t.Fatalf("info lines forwarded: %v", v)
	}
}

func TestBackupOverSSHFreezeFailedForwardsErrorLine(t *testing.T) {
	fakeSSH(t)
	t.Setenv("FAKE_SSH_STDOUT", writeVMA(t, testGuest()))
	t.Setenv("FAKE_SSH_LOG", fixtureLog(t, "freeze-failed.log"))
	r := runBackup(t, nil, sshArgs()...)
	assertResult(t, r, exitOK, false)
	lines := jsonLines(t, r.stderr)
	v := ofType(lines, "vzdump")
	if len(v) != 1 || v[0]["level"] != "error" || !strings.Contains(v[0]["line"].(string), "guest-fsfreeze-freeze") {
		t.Fatalf("vzdump lines %v", v)
	}
	c := ofType(lines, "consistency")
	if len(c) != 1 || c[0]["value"] != "crash" {
		t.Fatalf("consistency %v", c)
	}
	tr, _ := readRVS(t, r.stdout)
	if len(tr.VzdumpWarnings) != 1 {
		t.Fatalf("trailer vzdump warnings %v", tr.VzdumpWarnings)
	}
}

func TestBackupOverSSHBenignLinesLeaveNoWarnings(t *testing.T) {
	fakeSSH(t)
	t.Setenv("FAKE_SSH_STDOUT", writeVMA(t, testGuest()))
	log := filepath.Join(t.TempDir(), "benign.log")
	lines := "INFO: issuing guest-agent 'fs-freeze' command\n" +
		"Formatting '/tmp/vm-100-fleece-0.qcow2', fmt=qcow2 size=1048576\n" +
		"WARN: 'root@pam' does not have a configured email address\n" +
		"ERROR: could not notify via target `mail-to-root`: no recipients provided\n"
	if err := os.WriteFile(log, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_SSH_LOG", log)
	r := runBackup(t, nil, sshArgs()...)
	assertResult(t, r, exitOK, false)
	if v := ofType(jsonLines(t, r.stderr), "vzdump"); len(v) != 0 {
		t.Fatalf("benign lines forwarded: %v", v)
	}
	tr, _ := readRVS(t, r.stdout)
	if len(tr.VzdumpWarnings) != 0 {
		t.Fatalf("benign lines in trailer: %v", tr.VzdumpWarnings)
	}
}

func TestBackupOverSSHFleecingWarningExits5(t *testing.T) {
	fakeSSH(t)
	t.Setenv("FAKE_SSH_STDOUT", writeVMA(t, testGuest()))
	t.Setenv("FAKE_SSH_LOG", fixtureLog(t, "fleecing-warning.log"))
	r := runBackup(t, nil, sshArgs()...)
	res := assertResult(t, r, exitFleecingInactive, false)
	if !strings.Contains(res.Message, "fleecing") {
		t.Fatalf("message %q", res.Message)
	}
	assertNoManifest(t, r)
	assertIncomplete(t, r.stdout)
	if v := ofType(jsonLines(t, r.stderr), "vzdump"); len(v) != 1 || v[0]["level"] != "warn" {
		t.Fatalf("vzdump lines %v", v)
	}
}

func TestBackupOverSSHGuestFilesystemErrorExits10(t *testing.T) {
	fakeSSH(t)
	line := "ERROR: unable to freeze guest fs - failed to freeze /: Input/output error"
	log := filepath.Join(t.TempDir(), "freeze.log")
	if err := os.WriteFile(log, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_SSH_LOG", log)
	t.Setenv("FAKE_SSH_SLEEP", "60")
	r := runBackup(t, nil, sshArgs()...)
	res := assertResult(t, r, exitGuestFilesystem, false)
	if res.Message != "guest filesystem error: "+line {
		t.Fatalf("message %q", res.Message)
	}
	if res.Hint != "check the guest (fsck); the backup was not written" {
		t.Fatalf("hint %q", res.Hint)
	}
	assertNoManifest(t, r)
	assertIncomplete(t, r.stdout)
}

func TestBackupOverSSHFailureReasonDecidesRetry(t *testing.T) {
	for _, tc := range []struct {
		reason    string
		transient bool
	}{
		{"VM is locked (backup)", true},
		{"connection reset", true},
		{"disk image '/tmp/vm-100-fleece-0.qcow2' already exists", false},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			fakeSSH(t)
			log := filepath.Join(t.TempDir(), "failed.log")
			line := "ERROR: Backup of VM 100 failed - " + tc.reason
			if err := os.WriteFile(log, []byte(line+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("FAKE_SSH_LOG", log)
			t.Setenv("FAKE_SSH_EXIT", "255") // the explicit reason wins over the ssh status
			r := runBackup(t, nil, sshArgs()...)
			res := assertResult(t, r, exitSourceFailed, tc.transient)
			if !strings.Contains(res.Message, tc.reason) {
				t.Fatalf("message lacks failure reason: %q", res.Message)
			}
			assertNoManifest(t, r)
			assertIncomplete(t, r.stdout)
		})
	}
}

func TestBackupOverSSHFleecingWarningWhileStreaming(t *testing.T) {
	fakeSSH(t)
	b := testGuest()
	res, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	// only part of the stream, then the source hangs: the warning must abort it
	part := filepath.Join(t.TempDir(), "part.vma")
	os.WriteFile(part, res.Stream[:len(res.Stream)/2], 0o644)
	t.Setenv("FAKE_SSH_STDOUT", part)
	t.Setenv("FAKE_SSH_LOG", fixtureLog(t, "fleecing-warning.log"))
	t.Setenv("FAKE_SSH_SLEEP", "60")
	r := runBackup(t, nil, sshArgs()...)
	assertResult(t, r, exitFleecingInactive, false)
	assertNoManifest(t, r)
}

func TestBackupOverSSHExitStatus(t *testing.T) {
	cases := []struct {
		exit      string
		full      bool
		transient bool
	}{
		{"255", false, true}, // connection error, no stream
		{"1", true, false},   // vzdump failed after a complete-looking stream
		{"1", false, false},  // vzdump failed, no stream
		{"75", false, true},  // PVE busy
	}
	for _, c := range cases {
		t.Run(c.exit, func(t *testing.T) {
			fakeSSH(t)
			t.Setenv("FAKE_SSH_EXIT", c.exit)
			if c.full {
				t.Setenv("FAKE_SSH_STDOUT", writeVMA(t, testGuest()))
			}
			r := runBackup(t, nil, sshArgs()...)
			assertResult(t, r, exitSourceFailed, c.transient)
			assertNoManifest(t, r)
			assertIncomplete(t, r.stdout)
		})
	}
}

func TestBackupOverSSHCutStreamIsSourceFailure(t *testing.T) {
	fakeSSH(t)
	b := testGuest()
	res, _ := b.Build()
	part := filepath.Join(t.TempDir(), "part.vma")
	os.WriteFile(part, res.Stream[:len(res.Stream)/2], 0o644)
	t.Setenv("FAKE_SSH_STDOUT", part)
	t.Setenv("FAKE_SSH_EXIT", "255")
	r := runBackup(t, nil, sshArgs()...)
	assertResult(t, r, exitSourceFailed, true)
	assertNoManifest(t, r)
}

func TestBackupOverSSHIntegrityWinsOverKilledSource(t *testing.T) {
	fakeSSH(t)
	b := testGuest()
	b.Tamper = vmatest.Tamper{Kind: vmatest.TamperExtentMD5, Extent: 2}
	t.Setenv("FAKE_SSH_STDOUT", writeVMA(t, b))
	t.Setenv("FAKE_SSH_SLEEP", "60")
	r := runBackup(t, nil, sshArgs()...)
	assertResult(t, r, exitIntegrity, false)
	assertNoManifest(t, r)
}

// --- internal error ---

func TestPanicIsInternalError(t *testing.T) {
	commands["test-panic"] = func([]string, io.Reader, io.Writer, io.Writer) (exitCode, bool, error) {
		panic("boom")
	}
	defer delete(commands, "test-panic")
	code, _, stderr := invoke(t, "test-panic")
	if code != exitInternal {
		t.Fatalf("exit = %d, want 9", code)
	}
	r := lastResult(t, stderr)
	if r.Code != 9 || r.Transient || r.Status != "error" || !strings.Contains(r.Message, "boom") {
		t.Fatalf("result %+v", r)
	}
}
