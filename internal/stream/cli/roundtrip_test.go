package cli

// Task 4.4: determinism (dedup-friendliness), the multi-device round trip
// backup → RVS1 → to-vma checked by an independent reference VMA decoder, and
// the forced-overflow round trip.

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/norandom/restic-pve/internal/stream/rvs"
	"github.com/norandom/restic-pve/internal/stream/verify"
	"github.com/norandom/restic-pve/internal/stream/vma"
	"github.com/norandom/restic-pve/internal/stream/vma/vmatest"
)

// --- independent reference VMA decoder ---
//
// refDecode re-implements the VMA v1 layout from the format description with
// its own constants (it shares no code with internal/vma). It streams, checks
// the header md5, every extent's magic, reserved field, uuid (equal to the
// header uuid), md5 and blockinfo, and that every cluster of every device
// arrives exactly once. It computes sha256ct per device on its own.

const (
	refCluster   = 64 * 1024
	refBlock     = 4096
	refFixed     = 12288
	refExtHeader = 512
	refSlots     = 59
)

type refDevice struct {
	id     uint8
	name   string
	size   uint64
	seen   []bool
	hashes [][32]byte
}

func (d *refDevice) sha256ct() string {
	h := sha256.New()
	h.Write([]byte("RVS1-CT\x00"))
	var sz [8]byte
	binary.LittleEndian.PutUint64(sz[:], d.size)
	h.Write(sz[:])
	for _, c := range d.hashes {
		h.Write(c[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

type refStream struct {
	uuid        [16]byte
	configNames []string
	configs     map[string][]byte
	devices     map[uint8]*refDevice
	extents     int
}

type refCallback func(dev uint8, index uint32, data []byte) error

func refDecode(r io.Reader, cb refCallback) (*refStream, error) {
	be := binary.BigEndian
	fixed := make([]byte, refFixed)
	if _, err := io.ReadFull(r, fixed); err != nil {
		return nil, fmt.Errorf("ref: header: %w", err)
	}
	if !bytes.Equal(fixed[0:4], []byte("VMA\x00")) || be.Uint32(fixed[4:8]) != 1 {
		return nil, errors.New("ref: bad magic or version")
	}
	hsize := be.Uint32(fixed[56:60])
	if hsize < refFixed || hsize%512 != 0 || hsize > 64<<20 {
		return nil, fmt.Errorf("ref: header_size %d", hsize)
	}
	head := append(fixed, make([]byte, hsize-refFixed)...)
	if _, err := io.ReadFull(r, head[refFixed:]); err != nil {
		return nil, fmt.Errorf("ref: blob buffer: %w", err)
	}
	var stored [16]byte
	copy(stored[:], head[32:48])
	z := append([]byte(nil), head...)
	clear(z[32:48])
	if md5.Sum(z) != stored {
		return nil, errors.New("ref: header md5 mismatch")
	}
	s := &refStream{configs: map[string][]byte{}, devices: map[uint8]*refDevice{}}
	copy(s.uuid[:], head[8:24])
	boff, bsize := be.Uint32(head[48:52]), be.Uint32(head[52:56])
	if uint64(boff)+uint64(bsize) > uint64(hsize) {
		return nil, errors.New("ref: blob buffer outside the header")
	}
	blobs := head[boff : boff+bsize]
	blob := func(p uint32) ([]byte, error) {
		if p == 0 || int(p)+2 > len(blobs) {
			return nil, fmt.Errorf("ref: blob pointer %d", p)
		}
		n := int(binary.LittleEndian.Uint16(blobs[p:]))
		if int(p)+2+n > len(blobs) {
			return nil, fmt.Errorf("ref: blob %d overflows", p)
		}
		return blobs[int(p)+2 : int(p)+2+n], nil
	}
	cstr := func(p uint32) (string, error) {
		b, err := blob(p)
		if err != nil {
			return "", err
		}
		if len(b) == 0 || b[len(b)-1] != 0 {
			return "", errors.New("ref: string blob without NUL")
		}
		return string(b[:len(b)-1]), nil
	}
	for i := 0; i < 256; i++ {
		np, dp := be.Uint32(head[2044+4*i:]), be.Uint32(head[3068+4*i:])
		if np == 0 && dp == 0 {
			continue
		}
		name, err := cstr(np)
		if err != nil {
			return nil, err
		}
		data, err := blob(dp)
		if err != nil {
			return nil, err
		}
		s.configNames = append(s.configNames, name)
		s.configs[name] = append([]byte(nil), data...)
	}
	for id := 1; id < 255; id++ {
		e := head[4096+32*id : 4096+32*id+32]
		size := be.Uint64(e[8:16])
		if size == 0 {
			continue
		}
		name, err := cstr(be.Uint32(e[0:4]))
		if err != nil {
			return nil, err
		}
		n := (size + refCluster - 1) / refCluster
		s.devices[uint8(id)] = &refDevice{id: uint8(id), name: name, size: size, seen: make([]bool, n), hashes: make([][32]byte, n)}
	}

	ext := make([]byte, refExtHeader)
	cluster := make([]byte, refCluster)
	for {
		n, err := io.ReadFull(r, ext)
		if err == io.EOF && n == 0 {
			break
		}
		if err != nil {
			return s, fmt.Errorf("ref: extent %d header cut: %w", s.extents, err)
		}
		if !bytes.Equal(ext[0:4], []byte("VMAE")) {
			return s, fmt.Errorf("ref: extent %d magic", s.extents)
		}
		if be.Uint16(ext[4:6]) != 0 {
			return s, fmt.Errorf("ref: extent %d reserved", s.extents)
		}
		if !bytes.Equal(ext[8:24], s.uuid[:]) {
			return s, fmt.Errorf("ref: extent %d uuid differs from the header", s.extents)
		}
		var sum [16]byte
		copy(sum[:], ext[24:40])
		zz := append([]byte(nil), ext...)
		clear(zz[24:40])
		if md5.Sum(zz) != sum {
			return s, fmt.Errorf("ref: extent %d md5 mismatch", s.extents)
		}
		type ent struct {
			mask uint16
			dev  *refDevice
			idx  uint32
		}
		var ents []ent
		blocks := 0
		for slot := 0; slot < refSlots; slot++ {
			info := be.Uint64(ext[40+8*slot:])
			if info == 0 {
				continue
			}
			if (info>>40)&0xff != 0 {
				return s, fmt.Errorf("ref: extent %d slot %d reserved bits", s.extents, slot)
			}
			d := s.devices[uint8(info>>32)]
			idx := uint32(info)
			if d == nil || int(idx) >= len(d.seen) {
				return s, fmt.Errorf("ref: extent %d slot %d: bad device/cluster %#x", s.extents, slot, info)
			}
			if d.seen[idx] {
				return s, fmt.Errorf("ref: device %d cluster %d twice", d.id, idx)
			}
			d.seen[idx] = true
			e := ent{mask: uint16(info >> 48), dev: d, idx: idx}
			for m := e.mask; m != 0; m &= m - 1 {
				blocks++
			}
			ents = append(ents, e)
		}
		if int(be.Uint16(ext[6:8])) != blocks {
			return s, fmt.Errorf("ref: extent %d block_count", s.extents)
		}
		for _, e := range ents {
			clear(cluster)
			for b := 0; b < refCluster/refBlock; b++ {
				if e.mask&(1<<b) == 0 {
					continue
				}
				if _, err := io.ReadFull(r, cluster[b*refBlock:(b+1)*refBlock]); err != nil {
					return s, fmt.Errorf("ref: extent %d data cut: %w", s.extents, err)
				}
			}
			real := cluster[:min(refCluster, e.dev.size-uint64(e.idx)*refCluster)]
			e.dev.hashes[e.idx] = sha256.Sum256(real)
			if cb != nil {
				if err := cb(e.dev.id, e.idx, real); err != nil {
					return s, err
				}
			}
		}
		s.extents++
	}
	for _, d := range s.devices {
		for i, ok := range d.seen {
			if !ok {
				return s, fmt.Errorf("ref: device %d cluster %d missing", d.id, i)
			}
		}
	}
	return s, nil
}

// --- streaming synthetic devices (no full device in memory) ---

type genDevice struct {
	id   uint8
	name string
	size uint64
	seed uint64
}

func (g genDevice) clusters() uint32 { return uint32((g.size + refCluster - 1) / refCluster) }

// cluster writes the content of cluster idx into buf and returns it at its
// real length. Every 7th cluster (from 3) is zero, every 7th (from 5) has its
// even 4 KiB blocks zeroed, every 11th holds only one 4 KiB block of data.
func (g genDevice) cluster(idx uint32, buf []byte) []byte {
	n := min(uint64(refCluster), g.size-uint64(idx)*refCluster)
	out := buf[:n]
	if idx%7 == 3 {
		clear(out)
		return out
	}
	x := g.seed ^ (uint64(idx)+1)*0x9E3779B97F4A7C15
	for i := 0; i+8 <= len(out); i += 8 {
		x += 0x9E3779B97F4A7C15
		z := x
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		binary.LittleEndian.PutUint64(out[i:], z^(z>>31))
	}
	switch {
	case idx%7 == 5:
		for b := 0; b < len(out); b += 2 * refBlock {
			clear(out[b:min(b+refBlock, len(out))])
		}
	case idx%11 == 0:
		if len(out) > refBlock {
			clear(out[refBlock:])
		}
	}
	return out
}

func (g genDevice) sha256ct() string {
	d := &refDevice{size: g.size}
	buf := make([]byte, refCluster)
	for i := uint32(0); i < g.clusters(); i++ {
		d.hashes = append(d.hashes, sha256.Sum256(g.cluster(i, buf)))
	}
	return d.sha256ct()
}

// writeGenVMA writes a VMA file for the devices in the given arrival order.
func writeGenVMA(t *testing.T, path string, devs []genDevice, order []vmatest.ClusterRef) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	opts := vma.WriterOptions{CTime: 1760000000, Configs: []vma.ConfigBlob{vmatest.ExampleConfig()}}
	opts.UUID[0] = 0x42
	byID := map[uint8]genDevice{}
	for _, d := range devs {
		opts.Devices = append(opts.Devices, vma.DevInfo{ID: d.id, Name: d.name, Size: d.size})
		byID[d.id] = d
	}
	w, err := vma.NewWriter(bw, opts)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, refCluster)
	for _, r := range order {
		if err := w.WriteCluster(r.Dev, r.Index, byID[r.Dev].cluster(r.Index, buf)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := bw.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func refs(devs []genDevice) []vmatest.Device {
	var out []vmatest.Device
	for _, d := range devs {
		out = append(out, vmatest.Device{ID: d.id, Name: d.name, Size: d.size})
	}
	return out
}

// runToFile runs the CLI with stdout redirected to a file.
func runToFile(t *testing.T, out string, args ...string) cliRun {
	t.Helper()
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	var errb bytes.Buffer
	code := run(args, strings.NewReader(""), f, &errb)
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return cliRun{code: code, stderr: errb.String()}
}

// readTrailerFile streams an RVS1 file and returns its trailer.
func readTrailerFile(t *testing.T, path string) *rvs.Trailer {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r, err := rvs.NewReader(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, p, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, p); err != nil {
			t.Fatal(err)
		}
	}
	tr, err := r.Trailer()
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// TestMultiDeviceRoundTrip: a 300 MiB data disk (id 1), a 4 MiB efidisk-like
// device and a 64 KiB tpmstate-like device whose clusters arrive interleaved
// throughout the data disk (early clusters) → backup → RVS1 → verify →
// to-vma → independent reference decoder. Everything streams through files.
func TestMultiDeviceRoundTrip(t *testing.T) {
	devs := []genDevice{
		{id: 1, name: "drive-scsi0", size: 300 << 20, seed: 1},
		{id: 2, name: "drive-efidisk0", size: 4 << 20, seed: 2},
		{id: 3, name: "drive-tpmstate0-backup", size: 64 << 10, seed: 3},
	}
	dir := t.TempDir()
	in, stream, out := filepath.Join(dir, "in.vma"), filepath.Join(dir, "guest.rvs"), filepath.Join(dir, "out.vma")
	manifest := filepath.Join(dir, "guest.manifest.json")
	writeGenVMA(t, in, devs, vmatest.InterleavedOrder(refs(devs)))

	runToFile(t, stream, "backup", "--input", in, "--vmid", "100",
		"--manifest-out", manifest, "--ram-mib", "16", "--progress-interval", "1h").expect(t, exitOK)
	os.Remove(in)

	tr := readTrailerFile(t, stream)
	if tr.PatchCount != 0 {
		t.Fatalf("patch_count %d: the RAM tier should hold the small devices' early clusters", tr.PatchCount)
	}
	if tr.EarlyClusters == 0 {
		t.Fatal("the arrival order produced no early clusters")
	}
	m, err := verify.ReadManifestFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	want := map[uint8]string{}
	for _, d := range devs {
		want[d.id] = d.sha256ct()
	}
	for _, d := range m.Devices {
		if d.SHA256CT != want[d.ID] {
			t.Fatalf("backup digest of device %d %s, independent %s", d.ID, d.SHA256CT, want[d.ID])
		}
	}

	runWith(t, nil, "verify", "--in", stream, "--manifest", manifest).expect(t, exitOK)
	runWith(t, nil, "to-vma", "--in", stream, "--out", out, "--manifest", manifest).expect(t, exitOK)

	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	byID := map[uint8]genDevice{}
	for _, d := range devs {
		byID[d.id] = d
	}
	buf := make([]byte, refCluster)
	ref, err := refDecode(bufio.NewReaderSize(f, 1<<20), func(dev uint8, idx uint32, data []byte) error {
		if !bytes.Equal(data, byID[dev].cluster(idx, buf)) {
			return fmt.Errorf("device %d cluster %d content differs", dev, idx)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("reference decoder rejects the to-vma output: %v", err)
	}
	if len(ref.devices) != 3 || ref.extents == 0 {
		t.Fatalf("reference decoder saw %d devices, %d extents", len(ref.devices), ref.extents)
	}
	for _, d := range devs {
		rd := ref.devices[d.id]
		if rd == nil || rd.name != d.name || rd.size != d.size {
			t.Fatalf("device %d in to-vma output: %+v", d.id, rd)
		}
		if got := rd.sha256ct(); got != want[d.id] {
			t.Fatalf("device %d digest after the round trip %s, want %s", d.id, got, want[d.id])
		}
	}
	cfg := vmatest.ExampleConfig()
	if len(ref.configNames) != 1 || ref.configNames[0] != cfg.Name || !bytes.Equal(ref.configs[cfg.Name], cfg.Data) {
		t.Fatalf("config blob not byte-identical: %q", ref.configs[cfg.Name])
	}
	if !bytes.Contains(ref.configs[cfg.Name], []byte("#qmdump#map:tpmstate0:drive-tpmstate0-backup:")) {
		t.Fatal("#qmdump#map lines lost")
	}
}

// TestReferenceDecoderRejectsTruncatedVMA keeps the reference decoder honest:
// it must reject what to-vma leaves behind after a failure.
func TestReferenceDecoderRejectsTruncatedVMA(t *testing.T) {
	g := backupGuest(t)
	good := runWith(t, g.rvs, "to-vma")
	good.expect(t, exitOK)
	if _, err := refDecode(bytes.NewReader(good.stdout), nil); err != nil {
		t.Fatalf("reference decoder rejects a good stream: %v", err)
	}
	bad := runWith(t, flipData(t, g.rvs, 2), "to-vma")
	bad.expect(t, exitIntegrity)
	if _, err := refDecode(bytes.NewReader(bad.stdout), nil); err == nil {
		t.Fatal("reference decoder accepts a truncated to-vma output")
	}
	tampered := append([]byte(nil), good.stdout...)
	l, err := vmatest.Inspect(tampered)
	if err != nil {
		t.Fatal(err)
	}
	tampered[l.Extents[0].Offset+30] ^= 1 // extent md5
	if _, err := refDecode(bytes.NewReader(tampered), nil); err == nil || !strings.Contains(err.Error(), "md5") {
		t.Fatalf("reference decoder misses an extent md5 error: %v", err)
	}
}

// TestForcedOverflowRoundTrip: tiny caps force patch frames; the image still
// round-trips exactly through verify, to-vma and to-raw.
func TestForcedOverflowRoundTrip(t *testing.T) {
	b := testGuest()
	seq := vmatest.SequentialOrder(b.Devices)
	b.Order = append(append([]vmatest.ClusterRef{}, seq[len(seq)/2:]...), seq[:len(seq)/2]...)
	r := runBackup(t, nil, "--input", writeVMA(t, b), "--ram-mib", "1")
	assertResult(t, r, exitOK, false)
	tr, _ := readRVS(t, r.stdout)
	if tr.PatchCount == 0 {
		t.Fatal("tiny caps produced no patches")
	}
	prog := ofType(jsonLines(t, r.stderr), "progress")
	if p := prog[len(prog)-1]["patches"].(float64); uint64(p) != tr.PatchCount {
		t.Fatalf("progress reports %v patches, trailer %d", p, tr.PatchCount)
	}

	runWith(t, r.stdout, "verify", "--manifest", r.manifest).expect(t, exitOK)

	v := runWith(t, r.stdout, "to-vma", "--manifest", r.manifest)
	v.expect(t, exitOK)
	ref, err := refDecode(bytes.NewReader(v.stdout), nil)
	if err != nil {
		t.Fatalf("reference decoder rejects the to-vma output: %v", err)
	}
	checkVMA(t, v.stdout, b)
	for _, d := range tr.Devices {
		if got := ref.devices[d.ID].sha256ct(); got != d.SHA256CT {
			t.Fatalf("device %d digest %s, trailer %s", d.ID, got, d.SHA256CT)
		}
	}

	dir := t.TempDir()
	runWith(t, r.stdout, "to-raw", "--dir", dir, "--manifest", r.manifest).expect(t, exitOK)
	for _, d := range b.Devices {
		got, err := os.ReadFile(filepath.Join(dir, d.Name+".raw"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, b.Contents(d.ID)) {
			t.Fatalf("%s.raw differs after the overflow round trip", d.Name)
		}
	}
}

// --- determinism ---

// rvsFrames splits a complete RVS1 stream into frames.
func rvsFrames(t *testing.T, s []byte) []frameBytes {
	t.Helper()
	var out []frameBytes
	for len(s) > 0 {
		h, err := rvs.DecodeFrameHeader(s[:rvs.FrameHeaderSize])
		if err != nil {
			t.Fatal(err)
		}
		n := rvs.FrameHeaderSize + int(h.Length)
		out = append(out, frameBytes{h, s[:n]})
		s = s[n:]
	}
	return out
}

type frameBytes struct {
	h   rvs.FrameHeader
	raw []byte
}

// bodyOf returns the stream up to the trailer frame.
func bodyOf(t *testing.T, s []byte) []byte {
	t.Helper()
	fs := rvsFrames(t, s)
	last := fs[len(fs)-1]
	if last.h.Type != rvs.TypeTrailer {
		t.Fatalf("last frame is %v", last.h.Type)
	}
	return s[:len(s)-len(last.raw)]
}

func TestDeterministicAcrossArrivalOrders(t *testing.T) {
	orders := map[string]func([]vmatest.Device) []vmatest.ClusterRef{
		"sequential":  vmatest.SequentialOrder,
		"interleaved": vmatest.InterleavedOrder,
		"shuffled":    func(d []vmatest.Device) []vmatest.ClusterRef { return vmatest.ShuffledOrder(d, 7) },
	}
	var first []byte
	var firstName string
	n := 0
	for name, order := range orders {
		n++
		b := testGuest()
		b.Order = order(b.Devices)
		b.UUID[0] = byte(n) // per-run values differ
		b.CTime += int64(n)
		r := runBackup(t, nil, "--input", writeVMA(t, b), "--ram-mib", "64")
		assertResult(t, r, exitOK, false)
		tr, _ := readRVS(t, r.stdout)
		if tr.PatchCount != 0 {
			t.Fatalf("%s: overflow in the determinism case", name)
		}
		if first == nil {
			first, firstName = r.stdout, name
			continue
		}
		if !bytes.Equal(bodyOf(t, first), bodyOf(t, r.stdout)) {
			t.Fatalf("%s and %s: same contents gave different RVS1 bytes before the trailer", firstName, name)
		}
		if bytes.Equal(first, r.stdout) {
			t.Fatal("per-run values did not reach the trailer")
		}
	}
}

func TestOneDeviceChangeLeavesOtherDevicesIdentical(t *testing.T) {
	a := testGuest()
	ra := runBackup(t, nil, "--input", writeVMA(t, a), "--ram-mib", "64")
	assertResult(t, ra, exitOK, false)

	b := testGuest()
	tpm := &b.Devices[2] // id 3, tpmstate-like
	tpm.Data = append([]byte(nil), tpm.Data...)
	tpm.Data[1000] ^= 0xff
	b.UUID[0] = 0xee
	rb := runBackup(t, nil, "--input", writeVMA(t, b), "--ram-mib", "64")
	assertResult(t, rb, exitOK, false)

	fa, fb := rvsFrames(t, ra.stdout), rvsFrames(t, rb.stdout)
	if len(fa) != len(fb) {
		t.Fatalf("frame count %d vs %d", len(fa), len(fb))
	}
	changed := 0
	for i := range fa {
		if fa[i].h.Type == rvs.TypeTrailer {
			continue
		}
		same := bytes.Equal(fa[i].raw, fb[i].raw)
		if fa[i].h.Type == rvs.TypeData && fa[i].h.Dev == 3 {
			if same {
				t.Fatal("the changed device's frame is identical")
			}
			changed++
			continue
		}
		if !same {
			t.Fatalf("frame %d (%v dev %d offset %d) changed although its device did not", i, fa[i].h.Type, fa[i].h.Dev, fa[i].h.Offset)
		}
	}
	if changed != 1 {
		t.Fatalf("%d changed frames, want 1", changed)
	}
}
