package vma_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/norandom/restic-pve/internal/stream/vma"
	"github.com/norandom/restic-pve/internal/stream/vma/vmatest"
)

var fixedUUID = [16]byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x01}

const fixedCTime = 1760000000

// cluster returns a deterministic non-zero cluster with every 4 KiB block filled.
func cluster(seed int64) []byte {
	b := make([]byte, vma.ClusterSize)
	r := rand.New(rand.NewSource(seed))
	r.Read(b)
	for i := 0; i < vma.BlocksPerCluster; i++ {
		b[i*vma.BlockSize] = 0xA5 // never an all-zero block
	}
	return b
}

func inspect(t *testing.T, b []byte) *vmatest.Layout {
	t.Helper()
	l, err := vmatest.Inspect(b)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	return l
}

func newWriter(t *testing.T, buf *bytes.Buffer, devs []vma.DevInfo, cfgs []vma.ConfigBlob) *vma.Writer {
	t.Helper()
	w, err := vma.NewWriter(buf, vma.WriterOptions{UUID: fixedUUID, CTime: fixedCTime, Devices: devs, Configs: cfgs})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	return w
}

func checkExtents(t *testing.T, l *vmatest.Layout) {
	t.Helper()
	for i, e := range l.Extents {
		if e.Magic != vma.ExtentMagic {
			t.Errorf("extent %d: magic %q", i, e.Magic)
		}
		if !e.MD5OK() {
			t.Errorf("extent %d: md5 mismatch", i)
		}
		if e.UUID != l.UUID {
			t.Errorf("extent %d: uuid %x != header uuid %x", i, e.UUID, l.UUID)
		}
		if e.Reserved != 0 {
			t.Errorf("extent %d: reserved %d", i, e.Reserved)
		}
		if e.Slots > vma.WriterExtentClusters || e.Slots == 0 {
			t.Errorf("extent %d: %d slots", i, e.Slots)
		}
		blocks := 0
		for _, en := range e.Entries {
			if en.Reserved != 0 {
				t.Errorf("extent %d: blockinfo reserved bits set", i)
			}
			for m := en.Mask; m != 0; m &= m - 1 {
				blocks++
			}
		}
		if blocks != int(e.BlockCount) {
			t.Errorf("extent %d: block_count %d, mask bits %d", i, e.BlockCount, blocks)
		}
	}
}

func TestWriterHeaderLayout(t *testing.T) {
	conf := []byte("name: example\nscsi0: fast-ssd:vm-100-disk-0,size=128K\n#qmdump#map:scsi0:drive-scsi0:fast-ssd:raw:\n")
	cfgs := []vma.ConfigBlob{{Name: "qemu-server.conf", Data: conf}, {Name: "qemu-server.fw", Data: []byte("[OPTIONS]\nenable: 1\n")}}
	devs := []vma.DevInfo{{ID: 1, Name: "drive-scsi0", Size: 2 * vma.ClusterSize}, {ID: 3, Name: "drive-efidisk0", Size: vma.ClusterSize + 1000}}
	var buf bytes.Buffer
	w := newWriter(t, &buf, devs, cfgs)
	if w.UUID() != fixedUUID {
		t.Fatalf("UUID() = %x", w.UUID())
	}
	for i := uint32(0); i < 2; i++ {
		if err := w.WriteCluster(1, i, cluster(int64(i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.WriteCluster(3, 0, cluster(9)); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteCluster(3, 1, bytes.Repeat([]byte{7}, 1000)); err != nil { // tail cluster at its real length
		t.Fatal(err)
	}
	if err := w.Commit(); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	l := inspect(t, b)
	if l.Magic != vma.HeaderMagic || l.Version != 1 {
		t.Fatalf("magic %q version %d", l.Magic, l.Version)
	}
	if l.UUID != fixedUUID || l.CTime != fixedCTime {
		t.Fatalf("uuid %x ctime %d", l.UUID, l.CTime)
	}
	if !l.HeaderMD5OK() {
		t.Fatal("header md5 mismatch")
	}
	if l.BlobBufferOffset != vma.HeaderFixedSize {
		t.Fatalf("blob_buffer_offset %d", l.BlobBufferOffset)
	}
	want := uint32(vma.HeaderFixedSize) + (l.BlobBufferSize+511)/512*512
	if l.HeaderSize != want || l.HeaderSize%512 != 0 {
		t.Fatalf("header_size %d, want %d", l.HeaderSize, want)
	}
	if len(l.Configs) != 2 {
		t.Fatalf("configs %+v", l.Configs)
	}
	for i := range cfgs {
		if l.Configs[i].Name != cfgs[i].Name || !bytes.Equal(l.Configs[i].Data, cfgs[i].Data) {
			t.Errorf("config %d not verbatim: %q", i, l.Configs[i].Data)
		}
	}
	if len(l.Devices) != 2 || l.Devices[0] != devs[0] || l.Devices[1] != devs[1] {
		t.Fatalf("devices %+v", l.Devices)
	}
	// Blob lengths are little-endian and the first blob starts at position 1.
	be := binary.BigEndian
	ptrs := []uint32{be.Uint32(b[vma.OffConfigNames:]), be.Uint32(b[vma.OffDevInfo+1*vma.DevInfoSize:])}
	minPtr := ptrs[0]
	for _, p := range ptrs {
		minPtr = min(minPtr, p)
	}
	if minPtr != 1 {
		t.Fatalf("first blob at %d, want 1", minPtr)
	}
	p := ptrs[0]
	if got := binary.LittleEndian.Uint16(b[vma.HeaderFixedSize+p:]); got != uint16(len("qemu-server.conf")+1) {
		t.Fatalf("config name blob length (LE) = %d", got)
	}
	checkExtents(t, l)
	c := l.Contents()
	if !bytes.Equal(c[3][vma.ClusterSize:], bytes.Repeat([]byte{7}, 1000)) {
		t.Fatal("tail cluster content")
	}
}

func TestWriterFreshUUIDAndCTime(t *testing.T) {
	devs := []vma.DevInfo{{ID: 1, Name: "drive-scsi0", Size: vma.ClusterSize}}
	var a, b bytes.Buffer
	wa, err := vma.NewWriter(&a, vma.WriterOptions{Devices: devs})
	if err != nil {
		t.Fatal(err)
	}
	wb, err := vma.NewWriter(&b, vma.WriterOptions{Devices: devs})
	if err != nil {
		t.Fatal(err)
	}
	if wa.UUID() == ([16]byte{}) || wa.UUID() == wb.UUID() {
		t.Fatalf("uuids not fresh: %x %x", wa.UUID(), wb.UUID())
	}
	l := inspect(t, a.Bytes())
	if l.UUID != wa.UUID() {
		t.Fatal("header uuid differs from UUID()")
	}
	if d := time.Now().Unix() - l.CTime; d < -5 || d > 5 {
		t.Fatalf("ctime %d not current", l.CTime)
	}
	if !l.HeaderMD5OK() {
		t.Fatal("header md5")
	}
}

func TestWriterExtentsAtMost58Clusters(t *testing.T) {
	const n = 150
	devs := []vma.DevInfo{{ID: 1, Name: "drive-scsi0", Size: n * vma.ClusterSize}}
	var buf bytes.Buffer
	w := newWriter(t, &buf, devs, nil)
	want := make([]byte, 0, n*vma.ClusterSize)
	for i := uint32(0); i < n; i++ {
		c := cluster(int64(i))
		want = append(want, c...)
		if err := w.WriteCluster(1, i, c); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Commit(); err != nil {
		t.Fatal(err)
	}
	l := inspect(t, buf.Bytes())
	if len(l.Extents) != 3 || l.Extents[0].Slots != 58 || l.Extents[1].Slots != 58 || l.Extents[2].Slots != 34 {
		t.Fatalf("extent split: %d extents", len(l.Extents))
	}
	if l.TrailingBytes != 0 {
		t.Fatalf("trailing %d", l.TrailingBytes)
	}
	checkExtents(t, l)
	if !bytes.Equal(l.Contents()[1], want) {
		t.Fatal("device content differs")
	}
}

func TestWriterZeroClusters(t *testing.T) {
	devs := []vma.DevInfo{{ID: 2, Name: "drive-virtio1", Size: 3 * vma.ClusterSize}}
	var buf bytes.Buffer
	w := newWriter(t, &buf, devs, nil)
	partial := make([]byte, vma.ClusterSize)
	partial[5*vma.BlockSize+17] = 1 // only block 5 is non-zero
	if err := w.WriteCluster(2, 0, make([]byte, vma.ClusterSize)); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteCluster(2, 1, nil); err != nil { // nil = zero cluster
		t.Fatal(err)
	}
	if err := w.WriteCluster(2, 2, partial); err != nil {
		t.Fatal(err)
	}
	if err := w.Commit(); err != nil {
		t.Fatal(err)
	}
	l := inspect(t, buf.Bytes())
	checkExtents(t, l)
	e := l.Extents[0]
	if len(e.Entries) != 3 {
		t.Fatalf("entries %d", len(e.Entries))
	}
	if e.Entries[0].Mask != 0 || e.Entries[1].Mask != 0 {
		t.Fatalf("zero clusters masks %x %x", e.Entries[0].Mask, e.Entries[1].Mask)
	}
	if e.Entries[0].Dev != 2 || e.Entries[0].Index != 0 || e.Entries[1].Index != 1 {
		t.Fatalf("zero cluster blockinfo %+v", e.Entries[:2])
	}
	if e.Entries[2].Mask != 1<<5 || e.BlockCount != 1 {
		t.Fatalf("partial cluster mask %x block_count %d", e.Entries[2].Mask, e.BlockCount)
	}
	if !bytes.Equal(l.Contents()[2][2*vma.ClusterSize:], partial) {
		t.Fatal("partial cluster content")
	}
}

func TestWriterAcceptsAnyOrder(t *testing.T) {
	devs := []vma.DevInfo{{ID: 1, Name: "drive-scsi0", Size: 80 * vma.ClusterSize}, {ID: 2, Name: "drive-efidisk0", Size: 4 * vma.ClusterSize}}
	type ref struct {
		dev uint8
		idx uint32
	}
	var order []ref
	for _, d := range devs {
		for i := uint32(0); i < uint32(vma.Clusters(d.Size)); i++ {
			order = append(order, ref{d.ID, i})
		}
	}
	rand.New(rand.NewSource(42)).Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	var buf bytes.Buffer
	w := newWriter(t, &buf, devs, nil)
	want := map[uint8][]byte{1: make([]byte, devs[0].Size), 2: make([]byte, devs[1].Size)}
	for _, r := range order {
		c := cluster(int64(r.dev)<<32 | int64(r.idx))
		copy(want[r.dev][int(r.idx)*vma.ClusterSize:], c)
		if err := w.WriteCluster(r.dev, r.idx, c); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Commit(); err != nil {
		t.Fatal(err)
	}
	l := inspect(t, buf.Bytes())
	checkExtents(t, l)
	var got []ref
	for _, e := range l.Extents {
		for _, en := range e.Entries {
			got = append(got, ref{en.Dev, en.Index})
		}
	}
	if len(got) != len(order) {
		t.Fatalf("got %d entries, want %d", len(got), len(order))
	}
	for i := range got {
		if got[i] != order[i] {
			t.Fatalf("entry %d = %v, want arrival order %v", i, got[i], order[i])
		}
	}
	c := l.Contents()
	for id, w := range want {
		if !bytes.Equal(c[id], w) {
			t.Fatalf("device %d content differs", id)
		}
	}
}

func TestWriterHoldsBackFinalExtentUntilCommit(t *testing.T) {
	for _, n := range []int{1, 58, 59, 116, 117} {
		devs := []vma.DevInfo{{ID: 1, Name: "drive-scsi0", Size: uint64(n) * vma.ClusterSize}}
		var buf bytes.Buffer
		w := newWriter(t, &buf, devs, nil)
		for i := 0; i < n; i++ {
			if err := w.WriteCluster(1, uint32(i), cluster(int64(i))); err != nil {
				t.Fatal(err)
			}
		}
		total := (n + 57) / 58
		before := inspect(t, buf.Bytes())
		if len(before.Extents) != total-1 || before.TrailingBytes != 0 {
			t.Fatalf("n=%d: before Commit %d extents written, want %d", n, len(before.Extents), total-1)
		}
		if err := w.Commit(); err != nil {
			t.Fatal(err)
		}
		after := inspect(t, buf.Bytes())
		if len(after.Extents) != total {
			t.Fatalf("n=%d: after Commit %d extents, want %d", n, len(after.Extents), total)
		}
		checkExtents(t, after)
		if err := w.Commit(); err == nil {
			t.Fatalf("n=%d: second Commit succeeded", n)
		}
		if err := w.WriteCluster(1, 0, nil); err == nil {
			t.Fatalf("n=%d: WriteCluster after Commit succeeded", n)
		}
	}
}

func TestWriterAbortLeavesTruncatedStream(t *testing.T) {
	const n = 100
	devs := []vma.DevInfo{{ID: 1, Name: "drive-scsi0", Size: n * vma.ClusterSize}}
	var buf bytes.Buffer
	w := newWriter(t, &buf, devs, nil)
	for i := 0; i < n; i++ {
		if err := w.WriteCluster(1, uint32(i), cluster(int64(i))); err != nil {
			t.Fatal(err)
		}
	}
	w.Abort()
	l := inspect(t, buf.Bytes())
	if len(l.Extents) != 1 || l.Extents[0].Slots != 58 {
		t.Fatalf("aborted stream has %d extents, want only the first", len(l.Extents))
	}
	seen := 0
	for _, e := range l.Extents {
		seen += len(e.Entries)
	}
	if seen >= n {
		t.Fatal("aborted stream contains every cluster")
	}
	if err := w.Commit(); err == nil {
		t.Fatal("Commit after Abort succeeded")
	}
	if err := w.WriteCluster(1, 0, nil); err == nil {
		t.Fatal("WriteCluster after Abort succeeded")
	}
	if got := len(buf.Bytes()); got != int(l.HeaderSize)+vma.ExtentHeaderSize+58*vma.ClusterSize {
		t.Fatalf("Abort wrote more bytes: %d", got)
	}
}

func TestWriterRejectsBadClusters(t *testing.T) {
	devs := []vma.DevInfo{{ID: 1, Name: "drive-scsi0", Size: 2*vma.ClusterSize + 100}}
	cases := []struct {
		name  string
		dev   uint8
		index uint32
		data  []byte
	}{
		{"unknown device", 2, 0, nil},
		{"index out of range", 1, 3, nil},
		{"short data", 1, 0, make([]byte, 100)},
		{"long data", 1, 0, make([]byte, vma.ClusterSize+1)},
		{"wrong tail length", 1, 2, make([]byte, 99)},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		w := newWriter(t, &buf, devs, nil)
		if err := w.WriteCluster(c.dev, c.index, c.data); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
	var buf bytes.Buffer
	w := newWriter(t, &buf, devs, nil)
	if err := w.WriteCluster(1, 1, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteCluster(1, 1, nil); err == nil {
		t.Error("duplicate cluster accepted")
	}
	if err := w.WriteCluster(1, 2, make([]byte, vma.ClusterSize)); err != nil {
		t.Errorf("full-size tail cluster rejected: %v", err)
	}
	headerOnly := buf.Len()
	err := w.Commit()
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("Commit with a missing cluster: %v", err)
	}
	if buf.Len() != headerOnly {
		t.Fatal("failed Commit wrote the final extent")
	}
	w.Abort()
}

func TestWriterRejectsBadOptions(t *testing.T) {
	ok := vma.DevInfo{ID: 1, Name: "drive-scsi0", Size: vma.ClusterSize}
	cases := map[string]vma.WriterOptions{
		"no devices":        {},
		"device id 0":       {Devices: []vma.DevInfo{{ID: 0, Name: "drive-scsi0", Size: 1}}},
		"device id 255":     {Devices: []vma.DevInfo{{ID: 255, Name: "drive-scsi0", Size: 1}}},
		"duplicate id":      {Devices: []vma.DevInfo{ok, {ID: 1, Name: "drive-scsi1", Size: 1}}},
		"duplicate name":    {Devices: []vma.DevInfo{ok, {ID: 2, Name: "drive-scsi0", Size: 1}}},
		"zero size":         {Devices: []vma.DevInfo{{ID: 1, Name: "drive-scsi0"}}},
		"empty name":        {Devices: []vma.DevInfo{{ID: 1, Size: 1}}},
		"name with NUL":     {Devices: []vma.DevInfo{{ID: 1, Name: "drive\x00", Size: 1}}},
		"config too large":  {Devices: []vma.DevInfo{ok}, Configs: []vma.ConfigBlob{{Name: "qemu-server.conf", Data: make([]byte, vma.MaxBlobSize+1)}}},
		"config empty name": {Devices: []vma.DevInfo{ok}, Configs: []vma.ConfigBlob{{Data: []byte("x")}}},
		"too many configs":  {Devices: []vma.DevInfo{ok}, Configs: make([]vma.ConfigBlob, vma.MaxConfigs+1)},
	}
	for name, o := range cases {
		if _, err := vma.NewWriter(&bytes.Buffer{}, o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

type failWriter struct{ after int }

var errDisk = errors.New("disk full")

func (f *failWriter) Write(p []byte) (int, error) {
	if f.after < len(p) {
		return 0, errDisk
	}
	f.after -= len(p)
	return len(p), nil
}

func TestWriterPropagatesWriteErrors(t *testing.T) {
	devs := []vma.DevInfo{{ID: 1, Name: "drive-scsi0", Size: 100 * vma.ClusterSize}}
	if _, err := vma.NewWriter(&failWriter{after: 10}, vma.WriterOptions{Devices: devs}); !errors.Is(err, errDisk) {
		t.Fatalf("header write error: %v", err)
	}
	w, err := vma.NewWriter(&failWriter{after: 1 << 20}, vma.WriterOptions{Devices: devs})
	if err != nil {
		t.Fatal(err)
	}
	var got error
	for i := 0; i < 100 && got == nil; i++ {
		got = w.WriteCluster(1, uint32(i), cluster(int64(i)))
	}
	if !errors.Is(got, errDisk) {
		t.Fatalf("extent write error: %v", got)
	}
	if err := w.Commit(); !errors.Is(err, errDisk) {
		t.Fatalf("Commit after failure: %v", err)
	}
}
