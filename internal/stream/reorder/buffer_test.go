package reorder

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"testing"

	"github.com/norandom/restic-pve/internal/stream/vma"
	"github.com/norandom/restic-pve/internal/stream/vma/vmatest"
)

// recordingSink checks the Sink/HashSink contract and reconstructs every
// device's contents (in-order data, then patches overlaid).
type recordingSink struct {
	t      *testing.T
	devs   []vma.DevInfo
	pos    int // next expected (device, index) in cursor order
	dev    int
	out    map[uint8][]byte
	patch  map[[2]uint64][32]byte // patched positions → expected hash
	hashed *[2]uint64             // position announced by ClusterHash, awaiting InOrder(nil)
	inOrd  uint64
	failAt int // InOrder call number that fails (0 = never)
}

var errSinkFailed = errors.New("sink failed")

func newRecordingSink(t *testing.T, devs []vma.DevInfo) *recordingSink {
	s := &recordingSink{t: t, devs: devs, out: map[uint8][]byte{}, patch: map[[2]uint64][32]byte{}}
	for _, d := range devs {
		s.out[d.ID] = make([]byte, d.Size)
	}
	s.skipEmpty()
	return s
}

func (s *recordingSink) skipEmpty() {
	for s.dev < len(s.devs) && uint64(s.pos) >= vma.Clusters(s.devs[s.dev].Size) {
		s.dev++
		s.pos = 0
	}
}

func (s *recordingSink) devInfo(dev uint8) vma.DevInfo {
	for _, d := range s.devs {
		if d.ID == dev {
			return d
		}
	}
	s.t.Fatalf("unknown device %d", dev)
	return vma.DevInfo{}
}

// ahead reports whether (dev, index) has not been reached by InOrder yet.
func (s *recordingSink) ahead(dev uint8, index uint32) bool {
	if s.dev >= len(s.devs) {
		return false
	}
	cur := s.devs[s.dev]
	if dev != cur.ID {
		return dev > cur.ID
	}
	return int(index) >= s.pos
}

func (s *recordingSink) InOrder(dev uint8, index uint32, data []byte) error {
	s.inOrd++
	if s.failAt != 0 && int(s.inOrd) == s.failAt {
		return errSinkFailed
	}
	if s.dev >= len(s.devs) || dev != s.devs[s.dev].ID || int(index) != s.pos {
		s.t.Fatalf("InOrder(%d,%d) out of cursor order (want dev idx %d pos %d)", dev, index, s.dev, s.pos)
	}
	key := [2]uint64{uint64(dev), uint64(index)}
	_, patched := s.patch[key]
	if patched {
		if data != nil {
			s.t.Fatalf("InOrder(%d,%d) carries data for a patched position", dev, index)
		}
		if s.hashed == nil || *s.hashed != key {
			s.t.Fatalf("patched position (%d,%d) reached without ClusterHash right before", dev, index)
		}
	} else if s.hashed != nil {
		s.t.Fatalf("ClusterHash(%v) not followed by its InOrder", *s.hashed)
	}
	s.hashed = nil
	if data != nil {
		if len(data) != ClusterSize {
			s.t.Fatalf("InOrder data length %d", len(data))
		}
		copy(s.out[dev][uint64(index)*ClusterSize:], data)
	}
	s.pos++
	s.skipEmpty()
	return nil
}

func (s *recordingSink) Patch(dev uint8, index uint32, data []byte) error {
	key := [2]uint64{uint64(dev), uint64(index)}
	if !s.ahead(dev, index) {
		s.t.Fatalf("Patch(%d,%d) not ahead of the cursor", dev, index)
	}
	if _, dup := s.patch[key]; dup {
		s.t.Fatalf("Patch(%d,%d) twice", dev, index)
	}
	if len(data) != ClusterSize {
		s.t.Fatalf("Patch data length %d", len(data))
	}
	d := s.devInfo(dev)
	lo := uint64(index) * ClusterSize
	n := min(uint64(ClusterSize), d.Size-lo)
	s.patch[key] = sha256.Sum256(data[:n])
	copy(s.out[dev][lo:], data[:n])
	return nil
}

func (s *recordingSink) ClusterHash(dev uint8, index uint32, sum [32]byte) {
	key := [2]uint64{uint64(dev), uint64(index)}
	want, ok := s.patch[key]
	if !ok {
		s.t.Fatalf("ClusterHash(%d,%d) for an unpatched position", dev, index)
	}
	if sum != want {
		s.t.Fatalf("ClusterHash(%d,%d) mismatch", dev, index)
	}
	if s.dev >= len(s.devs) || dev != s.devs[s.dev].ID || int(index) != s.pos {
		s.t.Fatalf("ClusterHash(%d,%d) not at the cursor", dev, index)
	}
	s.hashed = &key
}

func (s *recordingSink) done() bool { return s.dev >= len(s.devs) && s.hashed == nil }

// --- fixtures ---

func clusterOf(b byte) []byte { return bytes.Repeat([]byte{b}, ClusterSize) }

func layoutOf(devs []vmatest.Device) Layout {
	var l Layout
	for _, d := range devs {
		l.Devices = append(l.Devices, vma.DevInfo{ID: d.ID, Name: d.Name, Size: d.Size})
	}
	return l
}

func testDevices() []vmatest.Device {
	return []vmatest.Device{
		vmatest.EFIDisk(1, 256*1024, 11),
		vmatest.DataDisk(2, "scsi0", 4<<20+1000, 12), // short final cluster
		vmatest.TPMState(4, 64*1024, 13),
	}
}

func totalClusters(devs []vmatest.Device) int64 {
	var n int64
	for _, d := range devs {
		n += int64(vma.Clusters(d.Size))
	}
	return n
}

// clusterFor returns what a vma.Reader yields for ref: a fresh ClusterSize
// slice (zero padded at the tail), or Zero with nil data.
func clusterFor(devs []vmatest.Device, ref vmatest.ClusterRef) vma.Cluster {
	for _, d := range devs {
		if d.ID != ref.Dev {
			continue
		}
		data := make([]byte, ClusterSize)
		lo := uint64(ref.Index) * ClusterSize
		if lo < uint64(len(d.Data)) {
			copy(data, d.Data[lo:min(lo+ClusterSize, uint64(len(d.Data)))])
		}
		if bytes.Equal(data, make([]byte, ClusterSize)) {
			return vma.Cluster{Dev: ref.Dev, Index: ref.Index, Zero: true}
		}
		return vma.Cluster{Dev: ref.Dev, Index: ref.Index, Data: data}
	}
	panic("unknown device")
}

func expected(devs []vmatest.Device) map[uint8][]byte {
	out := map[uint8][]byte{}
	for _, d := range devs {
		c := make([]byte, d.Size)
		copy(c, d.Data)
		out[d.ID] = c
	}
	return out
}

type runResult struct {
	sink  *recordingSink
	stats Stats
}

// run feeds order through a fresh Buffer and checks the result against the
// device contents and the caps.
func run(t *testing.T, devs []vmatest.Device, order []vmatest.ClusterRef, caps Caps) runResult {
	t.Helper()
	l := layoutOf(devs)
	sink := newRecordingSink(t, l.Devices)
	b, err := New(caps, l, sink, sink)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i, ref := range order {
		if err := b.Put(clusterFor(devs, ref)); err != nil {
			t.Fatalf("Put #%d %v: %v", i, ref, err)
		}
		checkCaps(t, b, caps)
	}
	if err := b.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if !sink.done() {
		t.Fatal("sink did not see every position")
	}
	for id, want := range expected(devs) {
		if !bytes.Equal(sink.out[id], want) {
			t.Fatalf("device %d contents differ", id)
		}
	}
	st := b.Stats()
	if st.RAMPeak > caps.RAMBytes {
		t.Fatalf("peaks %+v exceed caps %+v", st, caps)
	}
	if st.PatchCount != uint64(len(sink.patch)) {
		t.Fatalf("PatchCount %d, sink saw %d patches", st.PatchCount, len(sink.patch))
	}
	return runResult{sink, st}
}

// checkCaps asserts the accounting invariant on the live buffer.
func checkCaps(t *testing.T, b *Buffer, caps Caps) {
	t.Helper()
	if r := b.ramUsed(); r > caps.RAMBytes {
		t.Fatalf("RAM in use %d > cap %d", r, caps.RAMBytes)
	}
}

func bigCaps(t *testing.T) Caps {
	return Caps{RAMBytes: 64 << 20}
}

func nearShuffle(order []vmatest.ClusterRef, window int, seed int64) []vmatest.ClusterRef {
	out := append([]vmatest.ClusterRef(nil), order...)
	r := rand.New(rand.NewSource(seed))
	for lo := 0; lo < len(out); lo += window {
		hi := min(lo+window, len(out))
		r.Shuffle(hi-lo, func(i, j int) { out[lo+i], out[lo+j] = out[lo+j], out[lo+i] })
	}
	return out
}

// farJumps moves every 7th cluster from the back half of the sequence to the
// very beginning (copy-before-write writes far ahead of the read cursor),
// with the small devices interleaved throughout.
func farJumps(devs []vmatest.Device) []vmatest.ClusterRef {
	seq := vmatest.InterleavedOrder(devs)
	var early, rest []vmatest.ClusterRef
	for i, r := range seq {
		if i > len(seq)/2 && i%7 == 0 {
			early = append(early, r)
		} else {
			rest = append(rest, r)
		}
	}
	return append(early, rest...)
}

// --- tests ---

func TestBufferInOrderPassesThrough(t *testing.T) {
	devs := testDevices()
	caps := bigCaps(t)
	res := run(t, devs, vmatest.SequentialOrder(devs), caps)
	st := res.stats
	if st.EarlyClusters != 0 || st.PatchCount != 0 {
		t.Fatalf("stats %+v, want no buffering", st)
	}
	if st.RAMPeak != BitmapBytes(layoutOf(devs)) {
		t.Fatalf("RAMPeak %d, want bitmaps only (%d)", st.RAMPeak, BitmapBytes(layoutOf(devs)))
	}
}

func TestBufferShuffledNear(t *testing.T) {
	devs := testDevices()
	res := run(t, devs, nearShuffle(vmatest.SequentialOrder(devs), 16, 1), bigCaps(t))
	if res.stats.EarlyClusters == 0 || res.stats.PatchCount != 0 {
		t.Fatalf("stats %+v", res.stats)
	}
	if res.stats.RAMPeak <= BitmapBytes(layoutOf(devs)) {
		t.Fatalf("RAM tier unused: %+v", res.stats)
	}
}

func TestBufferFarJumpsAndInterleavedDevices(t *testing.T) {
	devs := testDevices()
	res := run(t, devs, farJumps(devs), bigCaps(t))
	if res.stats.EarlyClusters == 0 || res.stats.PatchCount != 0 {
		t.Fatalf("stats %+v", res.stats)
	}
}

func TestBufferFullyReversed(t *testing.T) {
	devs := testDevices()
	seq := vmatest.SequentialOrder(devs)
	rev := make([]vmatest.ClusterRef, len(seq))
	for i, r := range seq {
		rev[len(seq)-1-i] = r
	}
	res := run(t, devs, rev, bigCaps(t))
	if res.stats.PatchCount != 0 {
		t.Fatalf("stats %+v", res.stats)
	}
}
func TestBufferRAMCapTriggersPatchesRatherThanError(t *testing.T) {
	devs := testDevices()
	l := layoutOf(devs)
	caps := Caps{
		RAMBytes: BitmapBytes(l) + HashSlotBytes*totalClusters(devs) + 2*ClusterSize,
	}
	res := run(t, devs, farJumps(devs), caps)
	if res.stats.PatchCount == 0 {
		t.Fatalf("expected patches, stats %+v", res.stats)
	}
}

func TestBufferOverflowPatchesFarthest(t *testing.T) {
	// One device of 8 clusters, RAM for exactly one cluster plus hashes.
	dev := vmatest.DataDisk(1, "scsi0", 8*ClusterSize, 5)
	for i := range dev.Data { // no zero clusters in this test
		if dev.Data[i] == 0 {
			dev.Data[i] = 1
		}
	}
	devs := []vmatest.Device{dev}
	l := layoutOf(devs)
	caps := Caps{RAMBytes: BitmapBytes(l) + ClusterSize + 8*HashSlotBytes}
	order := []vmatest.ClusterRef{{Dev: 1, Index: 3}, {Dev: 1, Index: 6}, {Dev: 1, Index: 2}}
	sink := newRecordingSink(t, l.Devices)
	b, err := New(caps, l, sink, sink)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range order {
		if err := b.Put(clusterFor(devs, r)); err != nil {
			t.Fatal(err)
		}
	}
	// 3 buffered in RAM; 6 arrives (farther) → 6 patched; 2 arrives, 3 is
	// farthest buffered → 3 patched, 2 buffered.
	want := map[[2]uint64]bool{{1, 6}: true, {1, 3}: true}
	if len(sink.patch) != len(want) {
		t.Fatalf("patches %v, want %v", sink.patch, want)
	}
	for k := range want {
		if _, ok := sink.patch[k]; !ok {
			t.Fatalf("cluster %v not patched (patches %v)", k, sink.patch)
		}
	}
	for _, i := range []uint32{0, 1, 4, 5, 7} {
		if err := b.Put(clusterFor(devs, vmatest.ClusterRef{Dev: 1, Index: i})); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Finish(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sink.out[1], dev.Data) {
		t.Fatal("contents differ")
	}
	if st := b.Stats(); st.PatchCount != 2 || st.RAMPeak > caps.RAMBytes {
		t.Fatalf("stats %+v", st)
	}
}

func TestBufferZeroClustersOnlyInBitmap(t *testing.T) {
	devs := []vmatest.Device{{ID: 1, Name: "drive-scsi0", Size: 16 * ClusterSize}} // all zero
	l := layoutOf(devs)
	caps := Caps{RAMBytes: BitmapBytes(l)} // nothing else fits
	seq := vmatest.SequentialOrder(devs)
	rev := make([]vmatest.ClusterRef, len(seq))
	for i, r := range seq {
		rev[len(seq)-1-i] = r
	}
	res := run(t, devs, rev, caps)
	if res.stats.EarlyClusters != uint64(len(seq)-1) || res.stats.PatchCount != 0 || res.stats.RAMPeak != BitmapBytes(l) {
		t.Fatalf("stats %+v", res.stats)
	}
}

func TestBufferPropertyRandomOrdersNeverExceedCaps(t *testing.T) {
	devs := []vmatest.Device{
		vmatest.EFIDisk(1, 3*ClusterSize+100, 21),
		vmatest.DataDisk(3, "scsi0", 40*ClusterSize+7, 22),
		vmatest.TPMState(5, ClusterSize/2, 23),
	}
	l := layoutOf(devs)
	n := totalClusters(devs)
	seq := vmatest.SequentialOrder(devs)
	r := rand.New(rand.NewSource(42))
	patched := 0
	for iter := 0; iter < 150; iter++ {
		var order []vmatest.ClusterRef
		switch iter % 3 {
		case 0:
			order = vmatest.ShuffledOrder(devs, int64(iter))
		case 1:
			order = nearShuffle(seq, 2+r.Intn(20), int64(iter))
		default:
			order = farJumps(devs)
			order = nearShuffle(order, 1+r.Intn(6), int64(iter))
		}
		caps := Caps{
			RAMBytes: BitmapBytes(l) + HashSlotBytes*n + r.Int63n(6*ClusterSize),
		}
		t.Run(fmt.Sprintf("iter%d", iter), func(t *testing.T) {
			res := run(t, devs, order, caps)
			if res.stats.PatchCount > 0 {
				patched++
			}
		})
	}
	if patched == 0 {
		t.Fatal("property test never exercised the overflow path")
	}
}

func TestBufferThroughVMAReader(t *testing.T) {
	devs := testDevices()
	b := &vmatest.Builder{Devices: devs, Configs: []vma.ConfigBlob{vmatest.ExampleConfig()}, Order: farJumps(devs)}
	built, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	rd, err := vma.NewReader(bytes.NewReader(built.Stream))
	if err != nil {
		t.Fatal(err)
	}
	l := Layout{Devices: rd.Header().Devices}
	caps := Caps{RAMBytes: BitmapBytes(l) + HashSlotBytes*totalClusters(devs) + 3*ClusterSize}
	sink := newRecordingSink(t, l.Devices)
	buf, err := New(caps, l, sink, sink)
	if err != nil {
		t.Fatal(err)
	}
	for {
		c, err := rd.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := buf.Put(c); err != nil {
			t.Fatal(err)
		}
	}
	if err := buf.Finish(); err != nil {
		t.Fatal(err)
	}
	for _, d := range devs {
		if !bytes.Equal(sink.out[d.ID], b.Contents(d.ID)) {
			t.Fatalf("device %d differs", d.ID)
		}
	}
	if st := buf.Stats(); st.PatchCount == 0 || st.RAMPeak > caps.RAMBytes {
		t.Fatalf("stats %+v", st)
	}
}

func TestBufferFinishReportsMissingClusters(t *testing.T) {
	devs := testDevices()
	seq := vmatest.SequentialOrder(devs)
	l := layoutOf(devs)
	sink := newRecordingSink(t, l.Devices)
	b, err := New(bigCaps(t), l, sink, sink)
	if err != nil {
		t.Fatal(err)
	}
	// drop cluster 2 of device 2 and the last two clusters overall
	for i, r := range seq[:len(seq)-2] {
		if r.Dev == 2 && r.Index == 2 {
			continue
		}
		if err := b.Put(clusterFor(devs, r)); err != nil {
			t.Fatalf("Put #%d: %v", i, err)
		}
	}
	err = b.Finish()
	if !errors.Is(err, ErrMissingCluster) {
		t.Fatalf("Finish = %v, want ErrMissingCluster", err)
	}
	for _, s := range []string{"3 missing", "device 2 cluster 2"} {
		if !bytes.Contains([]byte(err.Error()), []byte(s)) {
			t.Fatalf("Finish error %q lacks %q", err, s)
		}
	}
}

func TestBufferRejectsInvalidClusters(t *testing.T) {
	devs := testDevices()
	l := layoutOf(devs)
	cases := []struct {
		name string
		prep []vma.Cluster
		c    vma.Cluster
	}{
		{"unknown device", nil, vma.Cluster{Dev: 3, Index: 0, Zero: true}},
		{"index out of range", nil, vma.Cluster{Dev: 1, Index: 4, Zero: true}},
		{"short data", nil, vma.Cluster{Dev: 1, Index: 0, Data: make([]byte, 10)}},
		{"behind cursor", []vma.Cluster{{Dev: 1, Index: 0, Zero: true}}, vma.Cluster{Dev: 1, Index: 0, Zero: true}},
		{"duplicate buffered", []vma.Cluster{{Dev: 1, Index: 2, Data: clusterOf(1)}}, vma.Cluster{Dev: 1, Index: 2, Data: clusterOf(1)}},
		{"duplicate zero", []vma.Cluster{{Dev: 2, Index: 5, Zero: true}}, vma.Cluster{Dev: 2, Index: 5, Data: clusterOf(1)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := newRecordingSink(t, l.Devices)
			b, err := New(bigCaps(t), l, sink, sink)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range tc.prep {
				if err := b.Put(c); err != nil {
					t.Fatal(err)
				}
			}
			if err := b.Put(tc.c); !errors.Is(err, ErrInvalidCluster) {
				t.Fatalf("Put = %v, want ErrInvalidCluster", err)
			}
		})
	}
}

func TestBufferDuplicateAfterPatchRejected(t *testing.T) {
	devs := []vmatest.Device{vmatest.DataDisk(1, "scsi0", 4*ClusterSize, 3)}
	l := layoutOf(devs)
	sink := newRecordingSink(t, l.Devices)
	b, err := New(Caps{RAMBytes: BitmapBytes(l) + 4*HashSlotBytes}, l, sink, sink)
	if err != nil {
		t.Fatal(err)
	}
	c := vma.Cluster{Dev: 1, Index: 3, Data: clusterOf(9)}
	if err := b.Put(c); err != nil {
		t.Fatal(err)
	}
	if b.Stats().PatchCount != 1 {
		t.Fatal("expected a patch")
	}
	if err := b.Put(c); !errors.Is(err, ErrInvalidCluster) {
		t.Fatalf("Put = %v, want ErrInvalidCluster", err)
	}
}
func TestBufferSinkErrorPropagates(t *testing.T) {
	devs := testDevices()
	l := layoutOf(devs)
	sink := newRecordingSink(t, l.Devices)
	sink.failAt = 2
	b, err := New(bigCaps(t), l, sink, sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Put(vma.Cluster{Dev: 1, Index: 1, Data: clusterOf(1)}); err != nil {
		t.Fatal(err)
	}
	if err := b.Put(vma.Cluster{Dev: 1, Index: 0, Data: clusterOf(2)}); !errors.Is(err, errSinkFailed) {
		t.Fatalf("Put = %v, want sink error", err)
	}
	if err := b.Put(vma.Cluster{Dev: 1, Index: 2, Data: clusterOf(3)}); !errors.Is(err, errSinkFailed) {
		t.Fatalf("Put after failure = %v, want sticky sink error", err)
	}
}

func TestBufferNilHashSink(t *testing.T) {
	devs := []vmatest.Device{vmatest.DataDisk(1, "scsi0", 4*ClusterSize, 3)}
	l := layoutOf(devs)
	sink := newRecordingSink(t, l.Devices)
	b, err := New(Caps{RAMBytes: BitmapBytes(l) + 4*HashSlotBytes}, l, &noHash{sink}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []uint32{3, 2, 1, 0} {
		if err := b.Put(clusterFor(devs, vmatest.ClusterRef{Dev: 1, Index: i})); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Finish(); err != nil {
		t.Fatal(err)
	}
	if b.Stats().PatchCount != 2 { // clusters 3 and 2; cluster 1 is a zero cluster
		t.Fatalf("stats %+v", b.Stats())
	}
}

// noHash forwards Sink calls and announces patched positions itself (the
// buffer has no HashSink), so the recording checks still apply.
type noHash struct{ r *recordingSink }

func (n *noHash) InOrder(dev uint8, index uint32, data []byte) error {
	if sum, ok := n.r.patch[[2]uint64{uint64(dev), uint64(index)}]; ok {
		n.r.ClusterHash(dev, index, sum)
	}
	return n.r.InOrder(dev, index, data)
}
func (n *noHash) Patch(dev uint8, index uint32, data []byte) error {
	return n.r.Patch(dev, index, data)
}

func TestBufferCapacityErrorWhenRAMCannotHoldHash(t *testing.T) {
	devs := []vmatest.Device{vmatest.DataDisk(1, "scsi0", 4*ClusterSize, 3)}
	l := layoutOf(devs)
	caps := Caps{RAMBytes: BitmapBytes(l) + HashSlotBytes}
	sink := newRecordingSink(t, l.Devices)
	b, err := New(caps, l, sink, sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Put(vma.Cluster{Dev: 1, Index: 3, Data: clusterOf(1)}); err != nil {
		t.Fatal(err)
	}
	err = b.Put(vma.Cluster{Dev: 1, Index: 2, Data: clusterOf(2)})
	if !errors.Is(err, ErrCapacity) {
		t.Fatalf("Put = %v, want ErrCapacity", err)
	}
	if b.ramData != 0 || b.hashBytes != HashSlotBytes || b.Stats().RAMInUse != caps.RAMBytes {
		t.Fatalf("capacity error without RAM entirely hash slots and bitmaps: %+v", b.Stats())
	}
	if err2 := b.Put(vma.Cluster{Dev: 1, Index: 0, Zero: true}); !errors.Is(err2, ErrCapacity) {
		t.Fatalf("Put after failure = %v, want sticky ErrCapacity", err2)
	}
	if err3 := b.Finish(); !errors.Is(err3, ErrCapacity) {
		t.Fatalf("Finish = %v, want sticky ErrCapacity", err3)
	}
	if st := b.Stats(); st.RAMPeak > caps.RAMBytes {
		t.Fatalf("RAM peak %d exceeds cap %d", st.RAMPeak, caps.RAMBytes)
	}
}

func TestNewValidates(t *testing.T) {
	devs := testDevices()
	l := layoutOf(devs)
	sink := newRecordingSink(t, l.Devices)
	unsorted := Layout{Devices: []vma.DevInfo{l.Devices[1], l.Devices[0]}}
	dup := Layout{Devices: []vma.DevInfo{l.Devices[0], l.Devices[0]}}
	cases := []struct {
		name string
		c    Caps
		l    Layout
		want error
	}{
		{"unsorted layout", bigCaps(t), unsorted, nil},
		{"duplicate device", bigCaps(t), dup, nil},
		{"empty layout", bigCaps(t), Layout{}, nil},
		{"negative ram", Caps{RAMBytes: -1}, l, nil},
		{"ram below bitmaps", Caps{RAMBytes: BitmapBytes(l) - 1}, l, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.c, tc.l, sink, sink)
			if err == nil {
				t.Fatal("New succeeded")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if _, err := New(bigCaps(t), l, nil, nil); err == nil {
		t.Fatal("nil sink accepted")
	}
}

func TestBufferEmptyDeviceIsSkipped(t *testing.T) {
	devs := []vmatest.Device{
		{ID: 1, Name: "drive-scsi0", Size: 0},
		vmatest.DataDisk(2, "scsi1", 3*ClusterSize, 4),
	}
	order := []vmatest.ClusterRef{{Dev: 2, Index: 2}, {Dev: 2, Index: 0}, {Dev: 2, Index: 1}}
	run(t, devs, order, bigCaps(t))
}

func TestBufferPutAfterFinish(t *testing.T) {
	devs := []vmatest.Device{vmatest.DataDisk(1, "scsi0", ClusterSize, 4)}
	l := layoutOf(devs)
	sink := newRecordingSink(t, l.Devices)
	b, err := New(bigCaps(t), l, sink, sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Put(clusterFor(devs, vmatest.ClusterRef{Dev: 1, Index: 0})); err != nil {
		t.Fatal(err)
	}
	if err := b.Finish(); err != nil {
		t.Fatal(err)
	}
	if err := b.Put(vma.Cluster{Dev: 1, Index: 0, Zero: true}); err == nil {
		t.Fatal("Put after Finish accepted")
	}
}

func TestBufferHashOfShortFinalCluster(t *testing.T) {
	// device ends 1000 bytes into cluster 2; patch cluster 2 and check the
	// hash covers only the real bytes (also verified by recordingSink.ClusterHash).
	dev := vmatest.DataDisk(1, "scsi0", 2*ClusterSize+1000, 8)
	devs := []vmatest.Device{dev}
	l := layoutOf(devs)
	order := []vmatest.ClusterRef{{Dev: 1, Index: 2}, {Dev: 1, Index: 0}, {Dev: 1, Index: 1}}
	res := run(t, devs, order, Caps{RAMBytes: BitmapBytes(l) + HashSlotBytes})
	if res.stats.PatchCount != 1 {
		t.Fatalf("stats %+v", res.stats)
	}
	want := sha256.Sum256(dev.Data[2*ClusterSize:])
	if res.sink.patch[[2]uint64{1, 2}] != want {
		t.Fatal("hash not over the truncated final cluster")
	}
}

func TestBufferStatsReportsCurrentUse(t *testing.T) {
	devs := testDevices()
	l := layoutOf(devs)
	caps := Caps{RAMBytes: BitmapBytes(l) + HashSlotBytes*totalClusters(devs) + 2*ClusterSize}
	sink := newRecordingSink(t, l.Devices)
	b, err := New(caps, l, sink, sink)
	if err != nil {
		t.Fatal(err)
	}
	var sawData, sawHashes bool
	for i, ref := range farJumps(devs) {
		if err := b.Put(clusterFor(devs, ref)); err != nil {
			t.Fatalf("Put #%d: %v", i, err)
		}
		st := b.Stats()
		want := b.ramData + int64(len(b.hashes))*HashSlotBytes + BitmapBytes(l)
		if st.RAMInUse != want || b.hashBytes != int64(len(b.hashes))*HashSlotBytes {
			t.Fatalf("Put #%d: RAMInUse %d, want %d; hash bytes %d for %d hashes", i, st.RAMInUse, want, b.hashBytes, len(b.hashes))
		}
		if st.RAMInUse > st.RAMPeak || st.RAMPeak > caps.RAMBytes {
			t.Fatalf("Put #%d: stats %+v exceed peaks or cap", i, st)
		}
		sawData = sawData || b.ramData > 0
		sawHashes = sawHashes || b.hashBytes > 0
	}
	if !sawData || !sawHashes {
		t.Fatalf("current use never showed data and hash slots (data %v, hashes %v)", sawData, sawHashes)
	}
	if err := b.Finish(); err != nil {
		t.Fatal(err)
	}
	if st := b.Stats(); st.RAMInUse != BitmapBytes(l) || b.hashBytes != 0 || b.ramData != 0 {
		t.Fatalf("after Finish stats %+v, want bitmaps only", st)
	}
}
