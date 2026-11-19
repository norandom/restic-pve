package verify

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/norandom/restic-pve/internal/stream/reorder"
	"github.com/norandom/restic-pve/internal/stream/rvs"
	"github.com/norandom/restic-pve/internal/stream/vma"
	"github.com/norandom/restic-pve/internal/stream/vma/vmatest"
)

const cs = vma.ClusterSize

// reference computes sha256ct straight from the definition in design.md.
func reference(content []byte) [32]byte {
	size := uint64(len(content))
	h := sha256.New()
	h.Write([]byte("RVS1-CT\x00"))
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], size)
	h.Write(b[:])
	for lo := uint64(0); lo < size; lo += cs {
		s := sha256.Sum256(content[lo:min(lo+cs, size)])
		h.Write(s[:])
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// padded returns cluster i as ClusterSize bytes (zero padded), or nil if all zero.
func padded(content []byte, i uint64) []byte {
	c := make([]byte, cs)
	copy(c, content[i*cs:min((i+1)*cs, uint64(len(content)))])
	if bytes.Equal(c, make([]byte, cs)) {
		return nil
	}
	return c
}

func digestInOrder(content []byte) *Digest {
	d := NewDigest(uint64(len(content)))
	for i := uint64(0); i < vma.Clusters(uint64(len(content))); i++ {
		d.Cluster(uint32(i), padded(content, i))
	}
	return d
}

func TestDigestMatchesDefinition(t *testing.T) {
	for _, size := range []int{0, 1, cs, cs + 1, 4<<20 + 1000} {
		content := vmatest.PatternData(size, int64(size))
		if got, want := digestInOrder(content).Sum(), reference(content); got != want {
			t.Fatalf("size %d: digest %x, want %x", size, got, want)
		}
	}
}

func TestDigestInOrderEqualsPatchMixed(t *testing.T) {
	content := vmatest.PatternData(4<<20+1000, 3)
	n := vma.Clusters(uint64(len(content)))
	d := NewDigest(uint64(len(content)))
	for i := uint64(0); i < n; i++ {
		lo, hi := i*cs, min((i+1)*cs, uint64(len(content)))
		switch {
		case i%3 == 0 || i == n-1: // patched, including the short final cluster
			d.ClusterHash(uint32(i), sha256.Sum256(content[lo:hi]))
		case i%3 == 1: // exact real length instead of padded
			d.Cluster(uint32(i), content[lo:hi])
		default:
			d.Cluster(uint32(i), padded(content, i))
		}
	}
	if d.Sum() != digestInOrder(content).Sum() {
		t.Fatal("patch-mixed input gave a different digest")
	}
}

func TestDigestDetectsChanges(t *testing.T) {
	content := vmatest.PatternData(1<<20+77, 9)
	base := digestInOrder(content).Sum()
	for _, off := range []int{0, cs - 1, cs * 7, len(content) - 1} {
		c := append([]byte(nil), content...)
		c[off] ^= 0x01
		if digestInOrder(c).Sum() == base {
			t.Fatalf("flipped byte at %d not detected", off)
		}
	}
	// the device size is bound in: one more trailing zero byte changes the digest
	longer := append(append([]byte(nil), content...), 0)
	if digestInOrder(longer).Sum() == base {
		t.Fatal("size change not detected")
	}
}

func TestDigestCountsAndZeroForms(t *testing.T) {
	content := vmatest.PatternData(10*cs+500, 4) // clusters 1 and 6 are zero
	d := digestInOrder(content)
	if d.ZeroClusters() != 2 {
		t.Fatalf("ZeroClusters %d", d.ZeroClusters())
	}
	if d.AllocatedBytes() != uint64(len(content))-2*cs {
		t.Fatalf("AllocatedBytes %d", d.AllocatedBytes())
	}
	// an explicit all-zero buffer and nil are the same cluster
	e := NewDigest(2*cs + 10)
	e.Cluster(0, make([]byte, cs))
	e.Cluster(1, nil)
	e.Cluster(2, make([]byte, 10))
	if e.Sum() != reference(make([]byte, 2*cs+10)) || e.ZeroClusters() != 3 || e.AllocatedBytes() != 0 {
		t.Fatal("zero cluster forms differ")
	}
}

func mustPanic(t *testing.T, name string, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("%s: no panic", name)
		}
	}()
	f()
}

func TestDigestMisuse(t *testing.T) {
	mustPanic(t, "incomplete Sum", func() { NewDigest(2 * cs).Sum() })
	mustPanic(t, "skipped cluster", func() { NewDigest(2*cs).Cluster(1, nil) })
	mustPanic(t, "beyond the end", func() {
		d := NewDigest(cs)
		d.Cluster(0, nil)
		d.Cluster(1, nil)
	})
	mustPanic(t, "short data", func() { NewDigest(2*cs).Cluster(0, make([]byte, 100)) })
	mustPanic(t, "hash out of order", func() { NewDigest(2*cs).ClusterHash(1, [32]byte{}) })
}

// --- Fingerprint (reorder Sink + HashSink) ---

func devicesFixture() []vmatest.Device {
	return []vmatest.Device{
		vmatest.EFIDisk(1, 256*1024, 21),
		vmatest.DataDisk(2, "scsi0", 3<<20+999, 22),
		{ID: 3, Name: "drive-scsi1", Size: 0},
		vmatest.TPMState(5, 64*1024, 23),
	}
}

func layout(devs []vmatest.Device) reorder.Layout {
	var l reorder.Layout
	for _, d := range devs {
		l.Devices = append(l.Devices, vma.DevInfo{ID: d.ID, Name: d.Name, Size: d.Size})
	}
	return l
}

func full(d vmatest.Device) []byte {
	c := make([]byte, d.Size)
	copy(c, d.Data)
	return c
}

func fingerprintVia(t *testing.T, devs []vmatest.Device, order []vmatest.ClusterRef, caps reorder.Caps) (*Fingerprint, reorder.Stats) {
	t.Helper()
	l := layout(devs)
	f := NewFingerprint(l)
	b, err := reorder.New(caps, l, f, f)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[uint8][]byte{}
	for _, d := range devs {
		byID[d.ID] = full(d)
	}
	for _, ref := range order {
		data := padded(byID[ref.Dev], uint64(ref.Index))
		if err := b.Put(vma.Cluster{Dev: ref.Dev, Index: ref.Index, Zero: data == nil, Data: data}); err != nil {
			t.Fatalf("Put %v: %v", ref, err)
		}
	}
	if err := b.Finish(); err != nil {
		t.Fatal(err)
	}
	return f, b.Stats()
}

func TestFingerprintThroughReorderBuffer(t *testing.T) {
	devs := devicesFixture()
	l := layout(devs)
	tiny := reorder.Caps{RAMBytes: reorder.BitmapBytes(l) + 4096 + 2*cs}
	f, st := fingerprintVia(t, devs, vmatest.ShuffledOrder(devs, 5), tiny)
	if st.PatchCount == 0 {
		t.Fatal("expected patches")
	}
	g, st2 := fingerprintVia(t, devs, vmatest.SequentialOrder(devs), tiny)
	if st2.PatchCount != 0 {
		t.Fatal("sequential order patched")
	}
	got, err := f.Devices()
	if err != nil {
		t.Fatal(err)
	}
	seq, _ := g.Devices()
	if len(got) != len(devs) {
		t.Fatalf("devices %+v", got)
	}
	for i, d := range devs {
		want := reference(full(d))
		if got[i].ID != d.ID || got[i].Name != d.Name || got[i].Size != d.Size ||
			got[i].SHA256CT != hex.EncodeToString(want[:]) {
			t.Fatalf("device %d: %+v, want digest %x", d.ID, got[i], want)
		}
		if got[i] != seq[i] {
			t.Fatalf("device %d: patched %+v differs from in-order %+v", d.ID, got[i], seq[i])
		}
	}
}

func TestFingerprintRejectsContractViolations(t *testing.T) {
	devs := devicesFixture()
	f := NewFingerprint(layout(devs))
	if err := f.InOrder(2, 0, nil); err == nil {
		t.Fatal("wrong device accepted")
	}
	f = NewFingerprint(layout(devs))
	f.ClusterHash(1, 0, [32]byte{})
	if err := f.InOrder(1, 0, make([]byte, cs)); err == nil {
		t.Fatal("data at a hashed (patched) position accepted")
	}
	f = NewFingerprint(layout(devs))
	if _, err := f.Devices(); err == nil {
		t.Fatal("incomplete fingerprint returned devices")
	}
	f = NewFingerprint(layout(devs))
	f.ClusterHash(1, 3, [32]byte{}) // not at the cursor
	if err := f.InOrder(1, 0, nil); err == nil {
		t.Fatal("misplaced ClusterHash accepted")
	}
}

func trailerFor(t *testing.T, devs []vmatest.Device) *rvs.Trailer {
	f, _ := fingerprintVia(t, devs, vmatest.SequentialOrder(devs), reorder.Caps{RAMBytes: 64 << 20})
	d, err := f.Devices()
	if err != nil {
		t.Fatal(err)
	}
	return &rvs.Trailer{Format: "RVS1", VMAUUID: "u", Devices: d, Tool: "vmastream test"}
}

func TestFingerprintCheckAgainstTrailer(t *testing.T) {
	devs := devicesFixture()
	tr := trailerFor(t, devs)
	f, _ := fingerprintVia(t, devs, vmatest.ShuffledOrder(devs, 8), reorder.Caps{RAMBytes: reorder.BitmapBytes(layout(devs)) + 4096})
	if err := f.Check(tr); err != nil {
		t.Fatalf("clean check: %v", err)
	}
	bad := *tr
	bad.Devices = append([]rvs.TrailerDevice(nil), tr.Devices...)
	bad.Devices[1].SHA256CT = strings.Repeat("0", 64)
	if err := f.Check(&bad); !errors.Is(err, rvs.ErrIntegrity) {
		t.Fatalf("digest mismatch: %v", err)
	}
}

// --- manifest ---

func sampleManifest(t *testing.T) *Manifest {
	tr := trailerFor(t, devicesFixture())
	return &Manifest{
		Schema: 1, VMID: 100, Alias: "example", Description: "Example guest", PVEName: "example",
		OSType: "l26", Consistency: "application", ConsistencyDetail: "",
		Devices: tr.Devices, Configs: ConfigDigests([]vma.ConfigBlob{vmatest.ExampleConfig()}),
		VMAUUID: "u", VMACTime: 1700000000, PatchCount: 0, BufferRAMPeak: 1,
		Started: "2026-10-11T01:00:00Z", Finished: "2026-10-11T02:00:00Z", Tool: "vmastream test",
	}
}

func TestManifestWriteReadRoundTrip(t *testing.T) {
	m := sampleManifest(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	if err := WriteManifestFile(path, m); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "manifest.json" {
		t.Fatalf("directory holds %v (temp file left?)", entries)
	}
	raw, _ := os.ReadFile(path)
	for _, key := range []string{`"schema":1`, `"vmid":100`, `"pve_name":"example"`, `"ostype":"l26"`,
		`"consistency_detail":""`, `"configs":[{"name":"qemu-server.conf","sha256":"`, `"image_snapshot_id":""`,
		`"image_bytes":0`, `"sha256ct":"`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("manifest lacks %s: %s", key, raw)
		}
	}
	back, err := ReadManifestFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.VMID != 100 || back.Alias != "example" || len(back.Devices) != 4 || back.Devices[1] != m.Devices[1] ||
		back.Configs[0] != m.Configs[0] || back.Started != m.Started {
		t.Fatalf("round trip: %+v", back)
	}
	sum := sha256.Sum256(vmatest.ExampleConfig().Data)
	if m.Configs[0].SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("config digest")
	}
	// overwrite keeps a single file
	if err := WriteManifestFile(path, m); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(path); !bytes.Equal(again, raw) {
		t.Fatal("manifest encoding not stable")
	}
}

func TestManifestWriteFailureLeavesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "manifest.json")
	if err := WriteManifestFile(path, sampleManifest(t)); err == nil {
		t.Fatal("write into a missing directory succeeded")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("file appeared")
	}
}

func TestReadManifestRejectsBadInput(t *testing.T) {
	for name, in := range map[string]string{
		"not json":     "{",
		"wrong schema": `{"schema":2,"devices":[]}`,
		"no schema":    `{"devices":[]}`,
		"trailing":     `{"schema":1} {}`,
	} {
		if _, err := ReadManifest(strings.NewReader(in)); !errors.Is(err, ErrManifest) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// fields added by backupctl later are tolerated
	if _, err := ReadManifest(strings.NewReader(`{"schema":1,"devices":[],"future":true}`)); err != nil {
		t.Fatalf("unknown field: %v", err)
	}
}

func TestCompare(t *testing.T) {
	m := sampleManifest(t)
	tr := trailerFor(t, devicesFixture())
	if err := Compare(tr, m); err != nil {
		t.Fatalf("matching manifest: %v", err)
	}
	cases := map[string]func(m *Manifest){
		"size":           func(m *Manifest) { m.Devices[1].Size += cs },
		"digest":         func(m *Manifest) { m.Devices[0].SHA256CT = strings.Repeat("f", 64) },
		"missing device": func(m *Manifest) { m.Devices = m.Devices[:3] },
		"extra device": func(m *Manifest) {
			m.Devices = append(m.Devices, rvs.TrailerDevice{ID: 9, Name: "drive-scsi9", Size: 1, SHA256CT: strings.Repeat("0", 64)})
		},
		"renamed device": func(m *Manifest) { m.Devices[1].Name = "drive-virtio0" },
		"duplicate id":   func(m *Manifest) { m.Devices = append(m.Devices, m.Devices[0]) },
		"schema":         func(m *Manifest) { m.Schema = 2 },
	}
	for name, mut := range cases {
		c := *m
		c.Devices = append([]rvs.TrailerDevice(nil), m.Devices...)
		mut(&c)
		err := Compare(tr, &c)
		if !errors.Is(err, ErrManifest) {
			t.Fatalf("%s: %v", name, err)
		}
		if errors.Is(err, rvs.ErrIntegrity) {
			t.Fatalf("%s: manifest mismatch must not be the integrity class", name)
		}
	}
	// allocated/zero counters and run data are not part of the comparison
	c := *m
	c.Devices = append([]rvs.TrailerDevice(nil), m.Devices...)
	c.Devices[0].ZeroClusters++
	c.VMAUUID = "other"
	if err := Compare(tr, &c); err != nil {
		t.Fatalf("informational fields compared: %v", err)
	}
	// a case-different hex digest is still the same digest
	c.Devices[0].SHA256CT = strings.ToUpper(c.Devices[0].SHA256CT)
	if err := Compare(tr, &c); err != nil {
		t.Fatalf("hex case: %v", err)
	}
}
