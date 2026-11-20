package run

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/norandom/restic-pve/internal/pieces"
	"github.com/norandom/restic-pve/internal/stream/blockmap"
	"github.com/norandom/restic-pve/internal/stream/changes"
	"github.com/norandom/restic-pve/internal/stream/cli"
	"github.com/norandom/restic-pve/internal/stream/vma"
	"github.com/norandom/restic-pve/internal/testfix"
)

// The fake host side of the stored runs ("image"): a guest with two disk images in the
// state folder, Proxmox's change record of each disk (record.json: the bitmap mode and the
// pieces written since the last successful provider job), and the two requests the backup
// machine sends: "changes <vmid> probe|use|new" (a provider job: the changes stream, the ack;
// the record moves on only after a success ack) and "dump <vmid>" (a VMA stream of the
// images, clusters in an order that is not the disk order). Files in the state folder change
// its behaviour: "notready" (exit 79), "lost" (the ssh session ends after the end frame,
// before the ack), "stopped" (vzdump in stop mode), "pve" (the Proxmox version).

type fakeDisk struct {
	ID   uint8  `json:"id"`
	Name string `json:"name"`
	Size uint64 `json:"size"`
}

type hostRecord struct {
	Bitmap map[string]string   `json:"bitmap"` // none | new | reuse; a missing disk is new
	Dirty  map[string][]uint64 `json:"dirty"`  // pieces written since the last successful job
}

const (
	efi    = "drive-efidisk0"
	scsi   = "drive-scsi0"
	efiLen = 540672
)

func readJSON(path string, v any) {
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, v)
	}
}

func writeJSON(path string, v any) {
	b, _ := json.Marshal(v)
	os.WriteFile(path, b, 0o644)
}

func has(dir, name string) bool { _, err := os.Stat(filepath.Join(dir, name)); return err == nil }

func logRequest(state, line string) {
	f, _ := os.OpenFile(filepath.Join(state, "requests"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	fmt.Fprintln(f, line)
	f.Close()
}

func imageHost(state string, args []string) int {
	req := args[6:]
	logRequest(state, strings.Join(req, " "))
	log := func(s string) { fmt.Fprintln(os.Stderr, s) }
	var disks []fakeDisk
	readJSON(filepath.Join(state, "disks.json"), &disks)
	out := bufio.NewWriterSize(os.Stdout, 1<<20)
	defer out.Flush()
	log("INFO: starting new backup job: vzdump 100 --mode snapshot")
	if has(state, "stopped") {
		log("INFO: backup mode: stop")
	} else {
		log("INFO: issuing guest-agent 'fs-freeze' command")
		log("INFO: issuing guest-agent 'fs-thaw' command")
	}
	if req[0] == "dump" {
		return imageDump(state, disks, out)
	}
	if has(state, "notready") {
		log("restic-pve: error: plug-in not ready: the storage restic-pve is not active; run restic-pve install on the Proxmox host")
		return 79
	}
	mode := req[2]
	var rec hostRecord
	readJSON(filepath.Join(state, "record.json"), &rec)
	pve := "9.2.24"
	if b, err := os.ReadFile(filepath.Join(state, "pve")); err == nil {
		pve = strings.TrimSpace(string(b))
	}
	var tok [16]byte
	rand.Read(tok[:])
	token := hex.EncodeToString(tok[:])
	h := changes.Header{Token: token, VMID: 100, Mode: mode, PVE: pve}
	all := true
	for _, d := range disks {
		bm := rec.Bitmap[d.Name]
		if bm == "" || mode == changes.ModeNew {
			bm = changes.BitmapNew
		}
		all = all && bm == changes.BitmapReuse
		h.Devices = append(h.Devices, changes.Device{ID: d.ID, Name: d.Name, Size: d.Size, Bitmap: bm, BitmapName: "restic-pve"})
	}
	enc := changes.NewEncoder(out)
	enc.Header(h)
	enc.Config("qemu-server.conf", []byte("name: web\n"))
	for i, d := range disks {
		recorded := h.Devices[i].Bitmap == changes.BitmapReuse && (mode == changes.ModeProbe || mode == changes.ModeUse && all)
		if !recorded {
			enc.Map(changes.Map{Dev: d.ID})
			continue
		}
		dirty := slices.Clone(rec.Dirty[d.Name])
		slices.Sort(dirty)
		dirty = slices.Compact(dirty)
		enc.Map(changes.NewMap(d.ID, d.Size, dirty))
		if mode != changes.ModeUse {
			continue
		}
		img, _ := os.Open(filepath.Join(state, d.Name+".img"))
		for _, p := range dirty {
			buf := make([]byte, pieces.PieceLen(d.Size, p))
			img.ReadAt(buf, int64(p*pieces.PieceSize))
			if err := enc.Piece(d.ID, p*pieces.PieceSize, buf); err != nil {
				log("restic-pve: error: " + err.Error())
				return 3
			}
		}
		img.Close()
	}
	enc.End()
	out.Flush()
	if has(state, "lost") {
		return 255
	}
	a, err := changes.ReadAck(os.Stdin, token, enc.Expected())
	if err != nil {
		if a.Verb == changes.VerbFail {
			logRequest(state, "ack fail")
		} else {
			logRequest(state, "ack none")
		}
		log("ERROR: Backup of VM 100 failed - the backup machine did not confirm the backup")
		return 2
	}
	logRequest(state, "ack "+string(a.Verb))
	// the job succeeded: Proxmox's record counts from this job, for every disk
	rec = hostRecord{Bitmap: map[string]string{}, Dirty: map[string][]uint64{}}
	for _, d := range disks {
		rec.Bitmap[d.Name] = changes.BitmapReuse
	}
	writeJSON(filepath.Join(state, "record.json"), rec)
	return 0
}

// imageDump writes the images as a VMA stream: the devices interleaved, each from both ends
// towards the middle, so pieces complete out of order and two are open at a time.
func imageDump(state string, disks []fakeDisk, out io.Writer) int {
	var devs []vma.DevInfo
	for _, d := range disks {
		devs = append(devs, vma.DevInfo{ID: d.ID, Name: d.Name, Size: d.Size})
	}
	conf := "#qmdump#map:efidisk0:drive-efidisk0:local:raw:\n#qmdump#map:scsi0:drive-scsi0:local:raw:\nname: web\n"
	w, err := vma.NewWriter(out, vma.WriterOptions{Devices: devs, Configs: []vma.ConfigBlob{{Name: "qemu-server.conf", Data: []byte(conf)}}})
	if err != nil {
		return 9
	}
	var most uint64
	for _, d := range disks {
		most = max(most, vma.Clusters(d.Size))
	}
	for k := uint64(0); k < most; k++ {
		for _, d := range disks {
			n := vma.Clusters(d.Size)
			if k >= n {
				continue
			}
			c := k / 2
			if k%2 == 1 {
				c = n - 1 - k/2
			}
			img, _ := os.Open(filepath.Join(state, d.Name+".img"))
			buf := make([]byte, min(vma.ClusterSize, d.Size-c*vma.ClusterSize))
			img.ReadAt(buf, int64(c*vma.ClusterSize))
			img.Close()
			if err := w.WriteCluster(d.ID, uint32(c), buf); err != nil {
				return 9
			}
		}
	}
	if err := w.Commit(); err != nil {
		return 9
	}
	return 0
}

// runFix is a guest with changed reads against a throwaway repository and the fake host.
type runFix struct {
	t                *testing.T
	dir, state, host string
	repo             *testfix.Repo
	env              Env
	disks            []fakeDisk
	info             map[string]any
	stderr           *bytes.Buffer
	seq              int64
}

func newRunFix(t *testing.T, scsiPieces uint64) *runFix {
	repo := testfix.NewRepo(t)
	dir := t.TempDir()
	f := &runFix{t: t, dir: dir, state: filepath.Join(dir, "state"), host: filepath.Join(dir, "host"), repo: repo, stderr: &bytes.Buffer{},
		disks: []fakeDisk{{ID: 1, Name: efi, Size: efiLen}, {ID: 2, Name: scsi, Size: scsiPieces * pieces.PieceSize}},
		info:  map[string]any{"status": "running", "disks": []string{"scsi0", "efidisk0"}, "pve_version": "9.2.24"}}
	for _, d := range []string{"bin", "host", "state"} {
		os.Mkdir(filepath.Join(dir, d), 0o755)
	}
	exe, _ := os.Executable()
	os.WriteFile(filepath.Join(dir, "bin", "ssh"), []byte("#!/bin/sh\nFAKE_HOST=image FAKE_STATE="+f.host+" exec "+exe+" \"$@\"\n"), 0o755)
	t.Setenv("PATH", filepath.Join(dir, "bin")+":/usr/bin:/bin")
	writeJSON(filepath.Join(f.host, "disks.json"), f.disks)
	for _, d := range f.disks {
		img, err := os.Create(filepath.Join(f.host, d.Name+".img"))
		if err != nil {
			t.Fatal(err)
		}
		img.Truncate(int64(d.Size))
		img.Close()
		for p := range pieces.Pieces(d.Size) {
			f.put(d.Name, p)
		}
	}
	f.env = Env{State: f.state, Stderr: f.stderr, Restic: &pieces.Restic{Bin: repo.Bin, Env: repo.Env()}, BatchBytes: max(8<<20, scsiPieces*pieces.PieceSize/4)}
	return f
}

// put writes 64 KiB of new random data at the start of a piece of the image; the rest of the
// piece stays as it was (zero at first).
func (f *runFix) put(dev string, p uint64) {
	img, err := os.OpenFile(filepath.Join(f.host, dev+".img"), os.O_WRONLY, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	defer img.Close()
	b := make([]byte, 64<<10)
	rand.Read(b)
	if _, err := img.WriteAt(b, int64(p*pieces.PieceSize)); err != nil {
		f.t.Fatal(err)
	}
}

// change writes pieces of a disk the way the guest does: Proxmox's record marks them.
func (f *runFix) change(dev string, ps ...uint64) {
	var rec hostRecord
	readJSON(filepath.Join(f.host, "record.json"), &rec)
	if rec.Dirty == nil {
		rec.Dirty = map[string][]uint64{}
	}
	for _, p := range ps {
		f.put(dev, p)
		rec.Dirty[dev] = append(rec.Dirty[dev], p)
	}
	writeJSON(filepath.Join(f.host, "record.json"), rec)
}

// stale changes a piece behind the record's back (a record that missed a write).
func (f *runFix) stale(dev string, p uint64) { f.put(dev, p) }

func (f *runFix) setBitmap(dev, mode string) {
	var rec hostRecord
	readJSON(filepath.Join(f.host, "record.json"), &rec)
	rec.Bitmap[dev] = mode
	writeJSON(filepath.Join(f.host, "record.json"), rec)
}

func (f *runFix) hostDirty(dev string) []uint64 {
	var rec hostRecord
	readJSON(filepath.Join(f.host, "record.json"), &rec)
	return rec.Dirty[dev]
}

func (f *runFix) flag(name string, on bool) {
	if on {
		os.WriteFile(filepath.Join(f.host, name), nil, 0o644)
	} else {
		os.Remove(filepath.Join(f.host, name))
	}
}

func (f *runFix) job(mode string) *Job {
	f.seq++
	j := map[string]any{"schema": 1, "mode": mode, "vmid": 100, "alias": "web", "source": "site1", "ssh_config": "/config/ssh_config",
		"ssh_host": "pve", "restic_host": "backup", "tags": []string{"source=site1", "instance=web", "unit=guest-web", "kind=guest-image", "vmid=100"},
		"info": f.info, "view_dir": filepath.Join(f.dir, "view"), "progress_interval_s": 1,
		"deadline": time.Now().Add(time.Hour).Format(time.RFC3339), "full_check_every": 7}
	p := filepath.Join(f.dir, fmt.Sprintf("job-%d.json", f.seq))
	writeJSON(p, j)
	job, err := LoadJob(p)
	if err != nil {
		f.t.Fatal(err)
	}
	return job
}

// run runs one stored run and returns its result and the requests the host saw.
func (f *runFix) run(mode string) (Result, []string) {
	f.t.Helper()
	os.Remove(filepath.Join(f.host, "requests"))
	f.stderr.Reset()
	res := Run(context.Background(), f.env, f.job(mode))
	b, _ := os.ReadFile(filepath.Join(f.host, "requests"))
	lines := strings.Split(strings.TrimSpace(f.stderr.String()), "\n")
	var last Result
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil || last.Type != "result" || last.Code != res.Code {
		f.t.Fatalf("the last stderr line is not the result: %q", lines[len(lines)-1])
	}
	return res, strings.Split(strings.TrimSpace(string(b)), "\n")
}

func (f *runFix) ok(res Result, kind string) string {
	f.t.Helper()
	if res.Code != 0 || res.Status != "ok" || res.Kind != kind || res.Snapshot == nil {
		f.t.Fatalf("want an accepted %s run: %+v\n%s", kind, res, f.stderr.String())
	}
	return *res.Snapshot
}

func (f *runFix) record() *pieces.RecordState {
	return pieces.LoadRecord(pieces.RecordFile(f.state, "web"), 100)
}

func (f *runFix) snapshots(tags ...string) []pieces.Snapshot {
	f.t.Helper()
	snaps, err := f.env.Restic.Snapshots(context.Background(), "backup", tags)
	if err != nil {
		f.t.Fatal(err)
	}
	return snaps
}

func (f *runFix) guestSnapshots() []string {
	var ids []string
	for _, s := range f.snapshots("kind=guest-image", "unit=guest-web") {
		ids = append(ids, s.ID)
	}
	return ids
}

func (f *runFix) noPartials() {
	f.t.Helper()
	if p := f.snapshots(pieces.PartialTag); len(p) != 0 {
		f.t.Fatalf("partial snapshots left: %v", p)
	}
}

// restored dumps snapshot id as a tar and checks that its pieces, in tar order, give every
// disk image byte for byte, and that its manifest's sha256pt matches.
func (f *runFix) restored(id string) *pieces.Manifest {
	f.t.Helper()
	root := filepath.Join(f.state, "pieces", "web", "vm-100")
	cmd := f.repo.Cmd("dump", "--archive", "tar", id, root)
	out, _ := cmd.StdoutPipe()
	cmd.Start()
	tr := tar.NewReader(out)
	sums := map[string]hash.Hash{}
	var m *pieces.Manifest
	var count int
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		rel := h.Name
		if i := strings.Index(rel, "vm-100/"); i >= 0 {
			rel = rel[i+len("vm-100/"):]
		}
		switch {
		case rel == pieces.ManifestName:
			if m, err = pieces.ReadManifest(tr); err != nil {
				f.t.Fatal(err)
			}
		case strings.HasPrefix(rel, "disks/") && h.Typeflag == tar.TypeReg:
			dev := strings.Split(rel, "/")[1]
			if sums[dev] == nil {
				sums[dev] = sha256.New()
			}
			io.Copy(sums[dev], tr)
			count++
		}
	}
	io.Copy(io.Discard, out)
	if err := cmd.Wait(); err != nil {
		f.t.Fatalf("restic dump %s: %v", id, err)
	}
	if m == nil {
		f.t.Fatalf("snapshot %s has no manifest", id)
	}
	for _, d := range f.disks {
		img, _ := os.ReadFile(filepath.Join(f.host, d.Name+".img"))
		want := sha256.Sum256(img)
		if s := sums[d.Name]; s == nil || !bytes.Equal(s.Sum(nil), want[:]) {
			f.t.Fatalf("snapshot %s: %s does not restore to the image", id, d.Name)
		}
	}
	var n uint64
	for _, d := range m.Devices {
		n += d.Pieces
	}
	if uint64(count) != n {
		f.t.Fatalf("snapshot %s: %d pieces in the tar, the manifest has %d", id, count, n)
	}
	return m
}

func (f *runFix) view() *blockmap.State {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, "view", blockmap.LiveFile))
	if err != nil {
		f.t.Fatal(err)
	}
	v, err := blockmap.Decode(b)
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

const (
	lineFirst   = "full read: first run with changed reads; the next run reads only changes"
	lineStopped = "full read: the guest is stopped"
	lineGone    = "full read: Proxmox has no record of changes (guest restarted, stopped or a disk changed); the next run reads only changes"
	lineCheck   = "full check: reading the whole disk to compare"
	linePVE     = "full read: the Proxmox version changed since the last backup; the next run reads only changes"
	lineLayout  = "full read: the last backup is not in the piece layout"
	lineReady   = "full read: the Proxmox plug-in is not ready (the storage restic-pve is not active); run restic-pve install on the Proxmox host"
	lineChecks  = "full read: the changed-blocks run did not pass its checks (C1); this run reads the whole disk"
)

func fallbackIs(t *testing.T, res Result, line string, expected bool) {
	t.Helper()
	if res.Fallback == nil || res.Fallback.Line != line || res.Fallback.Expected != expected {
		t.Fatalf("fallback %+v, want %q expected=%v", res.Fallback, line, expected)
	}
}

// 4.5: the first run arms and reads in full, then changed-blocks runs of 0 %, 1 % and 10 %
// store only what changed and restore byte for byte; a disk Proxmox reports new turns into a
// full read in the same run.
func TestRunChangedBlocks(t *testing.T) {
	f := newRunFix(t, 100)
	disk := uint64(efiLen + 100*pieces.PieceSize)
	res, reqs := f.run(ModeAuto)
	first := f.ok(res, "full")
	fallbackIs(t, res, lineFirst, true)
	eq(t, "requests", reqs, []string{"changes 100 new", "ack arm", "dump 100"})
	if r := f.record(); r.State != pieces.RecordBased || r.Base != first {
		t.Fatalf("record %+v", r)
	}
	if res.ReadBytes != disk || res.StoredBytes != disk || res.Message != lineFirst {
		t.Fatalf("%+v", res)
	}
	f.restored(first)
	for n, pct := range []uint64{0, 1, 10} {
		var ps []uint64
		for i := range pct {
			ps = append(ps, i*37%100)
		}
		f.change(scsi, ps...)
		res, reqs = f.run(ModeAuto)
		id := f.ok(res, "changes")
		eq(t, fmt.Sprintf("%d%%: requests", pct), reqs, []string{"changes 100 use", "ack ok"})
		read := pct * pieces.PieceSize
		if res.Fallback != nil || res.ReadBytes != read || res.StoredBytes != read || res.SkippedBytes != disk-read || res.Check != "none" {
			t.Fatalf("%d%%: %+v", pct, res)
		}
		if !strings.HasPrefix(res.Message, fmt.Sprintf("read %s, skipped %s (unchanged), ", size(read), size(disk-read))) {
			t.Fatalf("%d%%: message %q", pct, res.Message)
		}
		eq(t, "disks", res.Disks, []Disk{{efi, "reuse", 0}, {scsi, "reuse", read}})
		if r := f.record(); r.State != pieces.RecordBased || r.Base != id || r.RunsSinceCheck != n+1 {
			t.Fatalf("%d%%: record %+v", pct, r)
		}
		if len(f.hostDirty(scsi)) != 0 {
			t.Fatalf("%d%%: the host record did not move on", pct)
		}
		m := f.restored(id)
		if m.Kind != "changes" || m.Parent == nil || len(m.QmdumpMap) != 2 || m.Record == nil || m.Record.Disks[1].DirtyBytes != read {
			t.Fatalf("%d%%: manifest %+v", pct, m)
		}
		v := f.view()
		if v.Kind != blockmap.KindChanges || v.Status != blockmap.Done || v.Skipped != disk-read || !bytes.Contains(v.Map, []byte{'='}) ||
			pct > 0 && !bytes.Contains(v.Map, []byte{'#'}) || bytes.ContainsAny(v.Map, "?o") {
			t.Fatalf("%d%%: view %+v %s", pct, v, v.Map)
		}
		f.noPartials()
	}
	// the guest was restarted: Proxmox reports the disk new; the same run arms and reads in full
	f.setBitmap(scsi, changes.BitmapNew)
	f.stale(scsi, 50)
	res, reqs = f.run(ModeAuto)
	id := f.ok(res, "full")
	fallbackIs(t, res, lineGone, true)
	eq(t, "requests", reqs, []string{"changes 100 use", "ack arm", "dump 100"})
	eq(t, "disks", res.Disks, []Disk{{efi, "reuse", efiLen}, {scsi, "new", 100 * pieces.PieceSize}})
	if r := f.record(); r.State != pieces.RecordBased || r.Base != id {
		t.Fatalf("record %+v", r)
	}
	if v := f.view(); v.Kind != blockmap.KindFull {
		t.Fatalf("view kind %s", v.Kind)
	}
	f.restored(id)
	f.noPartials()
}

// 4.5: the ssh session ends after the stream, before the ack: nothing is accepted, the
// partial snapshots are forgotten, Proxmox's record still holds the change, and the next run
// reads it.
func TestRunLostBeforeAck(t *testing.T) {
	f := newRunFix(t, 8)
	res, _ := f.run(ModeAuto)
	first := f.ok(res, "full")
	f.change(scsi, 2, 3, 4)
	f.flag("lost", true)
	res, reqs := f.run(ModeAuto)
	if res.Code != CodeSource || !res.Transient || res.Snapshot != nil || res.Status != "error" {
		t.Fatalf("%+v\n%s", res, f.stderr.String())
	}
	eq(t, "requests", reqs, []string{"changes 100 use"})
	eq(t, "snapshots", f.guestSnapshots(), []string{first})
	f.noPartials()
	eq(t, "host record", f.hostDirty(scsi), []uint64{2, 3, 4})
	if r := f.record(); r.Base != first {
		t.Fatalf("record %+v", r)
	}
	f.flag("lost", false)
	res, reqs = f.run(ModeAuto)
	id := f.ok(res, "changes")
	eq(t, "requests", reqs, []string{"changes 100 use", "ack ok"})
	if res.ReadBytes != 3*pieces.PieceSize {
		t.Fatalf("%+v", res)
	}
	f.restored(id)
}

// 4.6: a first full read stores every piece; a full read with a parent (a stopped guest, no
// provider job) stores only the changed pieces; the record is based on the full read after
// an arm, and a stopped guest's record starts over.
func TestRunFullRead(t *testing.T) {
	f := newRunFix(t, 8)
	disk := uint64(efiLen + 8*pieces.PieceSize)
	res, _ := f.run(ModeAuto)
	first := f.ok(res, "full")
	if res.StoredBytes != disk || res.ReadBytes != disk {
		t.Fatalf("first run %+v", res)
	}
	if r := f.record(); r.State != pieces.RecordBased || r.Base != first || r.PVE != "9.2.24" || len(r.Disks) != 2 {
		t.Fatalf("record %+v", r)
	}
	m := f.restored(first)
	if m.Kind != "full" || m.Parent != nil || m.Record == nil || len(m.QmdumpMap) != 2 || m.Consistency.Value != "application" {
		t.Fatalf("manifest %+v", m)
	}
	// stopped: today's dump only, the changed pieces stored, everything read
	f.info["status"] = "stopped"
	f.flag("stopped", true)
	f.stale(scsi, 1)
	f.stale(scsi, 5)
	res, reqs := f.run(ModeAuto)
	id := f.ok(res, "full")
	fallbackIs(t, res, lineStopped, true)
	eq(t, "requests", reqs, []string{"dump 100"})
	if res.StoredBytes != 2*pieces.PieceSize || res.ReadBytes != disk {
		t.Fatalf("stopped %+v", res)
	}
	m = f.restored(id)
	if m.Kind != "full" || m.Parent == nil || *m.Parent != first || m.Record != nil || m.Consistency.Value != "stopped" {
		t.Fatalf("manifest %+v", m)
	}
	if r := f.record(); r.State != pieces.RecordNone || r.Base != "" {
		t.Fatalf("record after a stopped guest %+v", r)
	}
	// running again (Proxmox started the guest: no record): arm and read in full
	f.info["status"] = "running"
	f.flag("stopped", false)
	f.setBitmap(scsi, changes.BitmapNew)
	f.setBitmap(efi, changes.BitmapNew)
	res, reqs = f.run(ModeAuto)
	id = f.ok(res, "full")
	fallbackIs(t, res, lineGone, true)
	eq(t, "requests", reqs, []string{"changes 100 new", "ack arm", "dump 100"})
	if res.StoredBytes != 0 {
		t.Fatalf("nothing changed, yet %d bytes stored", res.StoredBytes)
	}
	if r := f.record(); r.State != pieces.RecordBased || r.Base != id {
		t.Fatalf("record %+v", r)
	}
	if v := f.view(); v.Kind != blockmap.KindFull || bytes.ContainsAny(v.Map, "?o") {
		t.Fatalf("view %+v", v)
	}
	f.restored(id)
	f.noPartials()
}

// 4.7: a full check on a clean fake says "full check: ok"; a piece the record missed gives
// the fault, the alert field and full runs until the fault is cleared.
func TestRunFullCheck(t *testing.T) {
	f := newRunFix(t, 8)
	res, _ := f.run(ModeAuto)
	s0 := f.ok(res, "full")
	f.change(scsi, 3)
	res, reqs := f.run(ModeFullCheck)
	id := f.ok(res, "check")
	fallbackIs(t, res, lineCheck, true)
	eq(t, "requests", reqs, []string{"dump 100", "changes 100 use", "ack ok"})
	if res.Check != "ok" || res.Message != "full check: ok" || res.Fault != nil {
		t.Fatalf("%+v", res)
	}
	today := time.Now().UTC().Format("2006-01-02")
	if r := f.record(); r.Base != id || r.RunsSinceCheck != 0 || r.LastCheck == nil || *r.LastCheck != (pieces.Check{Date: today, Result: "ok"}) {
		t.Fatalf("record %+v", r)
	}
	snaps := f.guestSnapshots()
	if len(snaps) != 3 || snaps[0] != s0 || snaps[2] != id {
		t.Fatalf("snapshots %v", snaps)
	}
	if m := f.restored(snaps[1]); m.Kind != "check" || *m.Parent != s0 {
		t.Fatalf("full read of the check %+v", m)
	}
	if m := f.restored(id); m.Kind != "changes" || *m.Parent != snaps[1] {
		t.Fatalf("changes part of the check %+v", m)
	}
	// the seventh run is a check without asking
	f.change(scsi, 1)
	res, _ = f.run(ModeAuto)
	f.ok(res, "changes")
	r := f.record()
	r.RunsSinceCheck = 6
	r.Save(pieces.RecordFile(f.state, "web"))
	res, _ = f.run(ModeAuto)
	f.ok(res, "check")
	eq(t, "check", res.Check, "ok")
	// a write the record missed
	f.stale(scsi, 6)
	f.change(scsi, 2)
	res, reqs = f.run(ModeFullCheck)
	sb := f.ok(res, "check")
	eq(t, "requests", reqs, []string{"dump 100", "changes 100 use", "ack fail"})
	if res.Check != "difference" || res.Fault == nil || fmt.Sprint(*res.Fault) != fmt.Sprintf("{%s [%s] 1}", today, scsi) ||
		!strings.Contains(res.Message, "--clear-fault web") {
		t.Fatalf("%+v", res)
	}
	if s := f.guestSnapshots(); s[len(s)-1] != sb {
		t.Fatal("the full read is not the newest backup")
	}
	f.restored(sb)
	f.noPartials()
	if r := f.record(); r.Fault == nil || r.LastCheck.Result != "difference" {
		t.Fatalf("record %+v", r)
	}
	// full runs until the fault is cleared
	for range 2 {
		res, reqs = f.run(ModeAuto)
		f.ok(res, "full")
		fallbackIs(t, res, fmt.Sprintf("full read: full check found a difference on %s; run ./backup.sh site1 --clear-fault web after looking at it", today), false)
		eq(t, "requests", reqs, []string{"dump 100"})
	}
	r = f.record()
	r.ClearFault()
	r.Save(pieces.RecordFile(f.state, "web"))
	res, reqs = f.run(ModeAuto)
	f.ok(res, "full")
	eq(t, "requests", reqs, []string{"changes 100 new", "ack arm", "dump 100"})
	f.change(scsi, 7)
	res, _ = f.run(ModeAuto)
	f.restored(f.ok(res, "changes"))
}

// 4.8: the fall backs and their lines.
func TestRunFallbacks(t *testing.T) {
	f := newRunFix(t, 8)
	res, _ := f.run(ModeAuto)
	f.ok(res, "full")

	// no working space: today's single stream, from inside the pieces command
	f.env.FreeSpace = func(string) (uint64, error) { return 100 << 20, nil }
	res, reqs := f.run(ModeAuto)
	id := f.ok(res, "full")
	fallbackIs(t, res, fmt.Sprintf("full read as a single stream: %s has 100 MiB free, pieces need 576 MiB", f.state), false)
	eq(t, "requests", reqs, []string{"dump 100"})
	f.env.FreeSpace = nil
	if v := f.view(); v.Kind != blockmap.KindFull || v.Status != blockmap.Done {
		t.Fatalf("view %+v", v)
	}
	rvs, err := f.repo.Run("dump", id, "/web/vm-100.rvs")
	if err != nil {
		t.Fatal(err)
	}
	var verr bytes.Buffer
	if code := cli.Main([]string{"verify", "--manifest", filepath.Join(f.state, "pieces", "web.rvs-manifest.json")}, bytes.NewReader(rvs), io.Discard, &verr); code != 0 {
		t.Fatalf("the single stream does not verify: %s", verr.String())
	}

	// the newest backup is that single stream
	res, reqs = f.run(ModeAuto)
	f.ok(res, "full")
	fallbackIs(t, res, lineLayout, true)
	eq(t, "requests", reqs, []string{"changes 100 new", "ack arm", "dump 100"})

	// plug-in not ready: a full read with the fix
	f.flag("notready", true)
	f.change(scsi, 1)
	res, reqs = f.run(ModeAuto)
	f.restored(f.ok(res, "full"))
	fallbackIs(t, res, lineReady, false)
	eq(t, "requests", reqs, []string{"changes 100 use", "dump 100"})
	f.flag("notready", false)
	res, _ = f.run(ModeAuto)
	f.ok(res, "full")
	fallbackIs(t, res, lineGone, true)

	// the Proxmox version changed
	f.info["pve_version"] = "9.2.25"
	os.WriteFile(filepath.Join(f.host, "pve"), []byte("9.2.25"), 0o644)
	res, reqs = f.run(ModeAuto)
	f.ok(res, "full")
	fallbackIs(t, res, linePVE, true)
	eq(t, "requests", reqs, []string{"changes 100 new", "ack arm", "dump 100"})

	// checks failed (a file restic stores that the run did not write): forget, arm, full read
	stray := func(root string) { os.WriteFile(filepath.Join(root, "disks", "stray"), []byte("x"), 0o644) }
	n := 0
	f.env.beforeBatch = func(root string) {
		if n++; n == 1 {
			stray(root)
		}
	}
	f.change(scsi, 2)
	before := f.guestSnapshots()
	res, reqs = f.run(ModeAuto)
	id = f.ok(res, "full")
	fallbackIs(t, res, lineChecks, false)
	eq(t, "requests", reqs, []string{"changes 100 use", "ack fail", "changes 100 new", "ack arm", "dump 100"})
	if v := f.view(); v.Kind != blockmap.KindFull {
		t.Fatalf("view %+v", v)
	}
	if s := f.guestSnapshots(); len(s) != len(before)+1 {
		t.Fatalf("snapshots %v after %v", s, before)
	}
	f.restored(id)
	f.noPartials()

	// failing twice: the run fails, nothing is stored
	f.env.beforeBatch = stray
	f.change(scsi, 3)
	before = f.guestSnapshots()
	res, reqs = f.run(ModeAuto)
	if res.Status != "error" || res.Code != CodeStream || res.Snapshot != nil || !strings.Contains(res.Message, "C1") {
		t.Fatalf("%+v", res)
	}
	eq(t, "requests", reqs, []string{"changes 100 use", "ack fail", "changes 100 new", "ack arm", "dump 100"})
	eq(t, "snapshots", f.guestSnapshots(), before)
	f.noPartials()
	f.env.beforeBatch = nil
	res, _ = f.run(ModeAuto)
	f.restored(f.ok(res, "full"))
}
