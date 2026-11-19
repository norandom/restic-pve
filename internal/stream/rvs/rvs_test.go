package rvs

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/norandom/restic-pve/internal/stream/reorder"
	"github.com/norandom/restic-pve/internal/stream/vma"
	"github.com/norandom/restic-pve/internal/stream/vma/vmatest"
)

// --- fixtures ---

func testDevices() []vmatest.Device {
	return []vmatest.Device{
		vmatest.EFIDisk(1, 256*1024, 11),
		vmatest.DataDisk(2, "scsi0", 4<<20+1000, 12), // short final cluster
		vmatest.TPMState(4, 64*1024, 13),
	}
}

func testHeader(devs []vmatest.Device, uuidByte byte, ctime int64) vma.Header {
	h := vma.Header{CTime: ctime}
	for i := range h.UUID {
		h.UUID[i] = uuidByte
	}
	for _, d := range devs {
		h.Devices = append(h.Devices, vma.DevInfo{ID: d.ID, Name: d.Name, Size: d.Size})
	}
	h.Configs = []vma.ConfigBlob{
		vmatest.ExampleConfig(),
		{Name: "qemu-server.fw", Data: []byte("[OPTIONS]\nenable: 0\n")},
	}
	return h
}

func layoutOf(h vma.Header) reorder.Layout { return reorder.Layout{Devices: h.Devices} }

// clusterOf returns a ClusterSize copy of cluster index (zero padded), or nil
// when the cluster is all zeros.
func clusterOf(d vmatest.Device, index uint32) []byte {
	data := make([]byte, ClusterSize)
	lo := uint64(index) * ClusterSize
	if lo < uint64(len(d.Data)) {
		copy(data, d.Data[lo:min(lo+ClusterSize, uint64(len(d.Data)))])
	}
	if bytes.Equal(data, make([]byte, ClusterSize)) {
		return nil
	}
	return data
}

func testTrailer(h vma.Header, run int) Trailer {
	t := Trailer{
		VMAUUID:        "00000000-0000-0000-0000-00000000000" + string(rune('0'+run)),
		VMACTime:       int64(1700000000 + run),
		PatchCount:     uint64(run),
		EarlyClusters:  uint64(10 * run),
		BufferRAMPeak:  int64(100 * run),
		Consistency:    "application",
		VzdumpWarnings: []string{"run <" + string(rune('0'+run)) + ">"},
		Tool:           "vmastream test",
	}
	for _, d := range h.Devices {
		t.Devices = append(t.Devices, TrailerDevice{
			ID: d.ID, Name: d.Name, Size: d.Size,
			SHA256CT: strings.Repeat("ab", 32), AllocatedBytes: d.Size / 2, ZeroClusters: uint64(run),
		})
	}
	return t
}

// writeSequential drives the writer directly in cursor order (no patches).
func writeSequential(t *testing.T, devs []vmatest.Device, h vma.Header, tr Trailer) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := NewWriter(&out, h)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for _, d := range devs {
		for i := uint64(0); i < vma.Clusters(d.Size); i++ {
			if err := w.InOrder(d.ID, uint32(i), clusterOf(d, uint32(i))); err != nil {
				t.Fatalf("InOrder(%d,%d): %v", d.ID, i, err)
			}
		}
	}
	if err := w.Close(tr); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return out.Bytes()
}

type noHash struct{}

func (noHash) ClusterHash(uint8, uint32, [32]byte) {}

// writeViaBuffer feeds the clusters in the given order through a reorder
// buffer into the writer.
func writeViaBuffer(t *testing.T, devs []vmatest.Device, h vma.Header, order []vmatest.ClusterRef, caps reorder.Caps, run int) ([]byte, reorder.Stats) {
	t.Helper()
	var out bytes.Buffer
	w, err := NewWriter(&out, h)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	b, err := reorder.New(caps, layoutOf(h), w, noHash{})
	if err != nil {
		t.Fatalf("reorder.New: %v", err)
	}
	byID := map[uint8]vmatest.Device{}
	for _, d := range devs {
		byID[d.ID] = d
	}
	for _, ref := range order {
		data := clusterOf(byID[ref.Dev], ref.Index)
		c := vma.Cluster{Dev: ref.Dev, Index: ref.Index, Zero: data == nil, Data: data}
		if err := b.Put(c); err != nil {
			t.Fatalf("Put %v: %v", ref, err)
		}
	}
	if err := b.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := w.Close(testTrailer(h, run)); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return out.Bytes(), b.Stats()
}

// decoded is what a full read of a stream yields.
type decoded struct {
	header   *Header
	contents map[uint8][]byte
	frames   []FrameHeader // data and patch frames in stream order
	patched  map[[2]uint64]bool
	trailer  *Trailer
}

// readAll reads a stream with Reader and rebuilds every device: patches are
// overlaid, data frames are copied except at patched positions, which must
// hold zero placeholders.
func readAll(t *testing.T, stream []byte) (*decoded, error) {
	t.Helper()
	r, err := NewReader(bytes.NewReader(stream))
	if err != nil {
		return nil, err
	}
	d := &decoded{header: r.Header(), contents: map[uint8][]byte{}, patched: map[[2]uint64]bool{}}
	sizes := map[uint8]uint64{}
	for _, dev := range d.header.Devices {
		d.contents[dev.ID] = make([]byte, dev.Size)
		sizes[dev.ID] = dev.Size
	}
	for {
		fh, payload, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return d, err
		}
		d.frames = append(d.frames, fh)
		buf, err := io.ReadAll(payload)
		if err != nil {
			return d, err
		}
		if uint64(len(buf)) != fh.Length {
			t.Fatalf("payload %d bytes, header says %d", len(buf), fh.Length)
		}
		dev := uint8(fh.Dev)
		switch fh.Type {
		case TypePatch:
			copy(d.contents[dev][fh.Offset:], buf)
			for off := fh.Offset; off < fh.Offset+fh.Length; off += ClusterSize {
				d.patched[[2]uint64{uint64(fh.Dev), off}] = true
			}
		case TypeData:
			for off := uint64(0); off < uint64(len(buf)); off += ClusterSize {
				c := buf[off:min(off+ClusterSize, uint64(len(buf)))]
				if d.patched[[2]uint64{uint64(fh.Dev), fh.Offset + off}] {
					if !bytes.Equal(c, make([]byte, len(c))) {
						t.Fatalf("patched position dev %d offset %d is not a zero placeholder", dev, fh.Offset+off)
					}
					continue
				}
				copy(d.contents[dev][fh.Offset+off:], c)
			}
		default:
			t.Fatalf("Next returned frame type %d", fh.Type)
		}
	}
	tr, err := r.Trailer()
	if err != nil {
		return d, err
	}
	d.trailer = tr
	return d, nil
}

func checkContents(t *testing.T, d *decoded, devs []vmatest.Device) {
	t.Helper()
	for _, dev := range devs {
		want := make([]byte, dev.Size)
		copy(want, dev.Data)
		if !bytes.Equal(d.contents[dev.ID], want) {
			t.Fatalf("device %d (%s) contents differ", dev.ID, dev.Name)
		}
	}
}

// rawFrame is one frame of a stream as raw bytes.
type rawFrame struct {
	h   FrameHeader
	raw []byte // header + payload
}

func splitFrames(t *testing.T, stream []byte) []rawFrame {
	t.Helper()
	var out []rawFrame
	for len(stream) > 0 {
		h, err := DecodeFrameHeader(stream[:FrameHeaderSize])
		if err != nil {
			t.Fatalf("DecodeFrameHeader: %v", err)
		}
		n := FrameHeaderSize + int(h.Length)
		out = append(out, rawFrame{h, stream[:n]})
		stream = stream[n:]
	}
	return out
}

func joinFrames(fs []rawFrame) []byte {
	var b []byte
	for _, f := range fs {
		b = append(b, f.raw...)
	}
	return b
}

// reframe re-encodes a frame with a modified header (valid CRC) and payload.
func reframe(h FrameHeader, payload []byte) rawFrame {
	enc := h.Encode()
	return rawFrame{h, append(enc[:], payload...)}
}

func expectIntegrity(t *testing.T, err error, kind ErrorKind) {
	t.Helper()
	if err == nil {
		t.Fatalf("want %v error, got nil", kind)
	}
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("error %v is not of the integrity class", err)
	}
	var fe *FormatError
	if !errors.As(err, &fe) || fe.Kind != kind {
		t.Fatalf("want kind %v, got %v", kind, err)
	}
}

// --- frame header ---

func TestFrameHeaderLayout(t *testing.T) {
	h := FrameHeader{Type: TypeData, Dev: 2, Offset: 3 * FrameBytes, Length: 12345}
	b := h.Encode()
	if string(b[0:4]) != "RVSF" || b[4] != 3 || b[5] != 0 {
		t.Fatalf("magic/type/flags wrong: % x", b[:8])
	}
	le := binary.LittleEndian
	if le.Uint32(b[8:]) != 2 || le.Uint64(b[16:]) != 3*FrameBytes || le.Uint64(b[24:]) != 12345 {
		t.Fatalf("dev/offset/length wrong: % x", b[:32])
	}
	if !bytes.Equal(b[32:56], make([]byte, 24)) || !bytes.Equal(b[60:64], make([]byte, 4)) {
		t.Fatal("reserved bytes not zero")
	}
	if got := le.Uint32(b[56:]); got != crc32c(b[:56]) {
		t.Fatalf("hcrc %08x, want CRC-32C of bytes 0-55", got)
	}
	back, err := DecodeFrameHeader(b[:])
	if err != nil || back != h {
		t.Fatalf("decode: %+v %v", back, err)
	}
	for _, i := range []int{0, 4, 9, 20, 30, 40, 57} {
		c := b
		c[i] ^= 0x40
		if _, err := DecodeFrameHeader(c[:]); err == nil {
			t.Fatalf("flip at byte %d accepted", i)
		}
	}
	c := b
	c[61] = 1 // reserved after hcrc, not covered by the CRC
	if _, err := DecodeFrameHeader(c[:]); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("non-zero trailing reserved accepted: %v", err)
	}
}

// --- round trip ---

func TestRoundTripSequential(t *testing.T) {
	devs := testDevices()
	h := testHeader(devs, 0x11, 1)
	tr := testTrailer(h, 1)
	stream := writeSequential(t, devs, h, tr)
	d, err := readAll(t, stream)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	checkContents(t, d, devs)
	if len(d.patched) != 0 {
		t.Fatal("unexpected patches")
	}
	if len(d.header.Devices) != 3 || d.header.Devices[1] != h.Devices[1] {
		t.Fatalf("device table %+v", d.header.Devices)
	}
	if len(d.header.Configs) != 2 || d.header.Configs[0].Name != "qemu-server.conf" ||
		!bytes.Equal(d.header.Configs[0].Data, h.Configs[0].Data) || !bytes.Equal(d.header.Configs[1].Data, h.Configs[1].Data) {
		t.Fatal("configs not verbatim")
	}
	want := tr
	want.Format = "RVS1"
	gotJ, _ := json.Marshal(d.trailer)
	wantJ, _ := json.Marshal(want)
	if !bytes.Equal(gotJ, wantJ) {
		t.Fatalf("trailer\n got %s\nwant %s", gotJ, wantJ)
	}
	// one data frame per device here; lengths are the device sizes
	data := d.frames
	if len(data) != 3 || data[1].Dev != 2 || data[1].Offset != 0 || data[1].Length != 4<<20+1000 {
		t.Fatalf("data frames %+v", data)
	}
}

func TestRoundTripWithPatches(t *testing.T) {
	devs := testDevices()
	h := testHeader(devs, 0x22, 2)
	order := vmatest.ShuffledOrder(devs, 7)
	caps := reorder.Caps{RAMBytes: reorder.BitmapBytes(layoutOf(h)) + 4096 + 4*ClusterSize}
	stream, st := writeViaBuffer(t, devs, h, order, caps, 2)
	if st.PatchCount == 0 {
		t.Fatal("expected patches with tiny caps")
	}
	d, err := readAll(t, stream)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	checkContents(t, d, devs)
	if uint64(len(d.patched)) != st.PatchCount {
		t.Fatalf("%d patched positions, buffer reports %d", len(d.patched), st.PatchCount)
	}
	// every patch frame precedes the data frame covering it
	seen := map[[2]uint64]bool{}
	for _, f := range d.frames {
		switch f.Type {
		case TypeData:
			seen[[2]uint64{uint64(f.Dev), f.Offset}] = true
		case TypePatch:
			if seen[[2]uint64{uint64(f.Dev), f.Offset / FrameBytes * FrameBytes}] {
				t.Fatalf("patch %+v after its data frame", f)
			}
		}
	}
}

func TestMultipleDataFrames(t *testing.T) {
	size := uint64(FrameBytes + 3*ClusterSize + 100)
	data := make([]byte, size)
	for _, off := range []uint64{0, 5 * ClusterSize, FrameBytes - 1, FrameBytes, FrameBytes + 2*ClusterSize + 7, size - 1} {
		data[off] = byte(off%251) + 1
	}
	devs := []vmatest.Device{
		{ID: 1, Name: "drive-scsi0", Size: size, Data: data},
		vmatest.TPMState(3, 64*1024, 5),
	}
	h := testHeader(devs, 1, 1)
	h.Configs = nil
	// patch one cluster in the second frame while the first frame is open
	var out bytes.Buffer
	w, err := NewWriter(&out, h)
	if err != nil {
		t.Fatal(err)
	}
	patchIdx := uint32(FrameBytes/ClusterSize + 2)
	if err := w.InOrder(1, 0, clusterOf(devs[0], 0)); err != nil {
		t.Fatal(err)
	}
	if err := w.Patch(1, patchIdx, clusterOf(devs[0], patchIdx)); err != nil {
		t.Fatal(err)
	}
	for i := uint32(1); i < uint32(vma.Clusters(size)); i++ {
		c := clusterOf(devs[0], i)
		if i == patchIdx {
			c = nil
		}
		if err := w.InOrder(1, i, c); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.InOrder(3, 0, clusterOf(devs[1], 0)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(testTrailer(h, 1)); err != nil {
		t.Fatal(err)
	}
	d, err := readAll(t, out.Bytes())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	checkContents(t, d, devs)
	want := []FrameHeader{
		{TypePatch, 1, uint64(patchIdx) * ClusterSize, ClusterSize},
		{TypeData, 1, 0, FrameBytes},
		{TypeData, 1, FrameBytes, 3*ClusterSize + 100},
		{TypeData, 3, 0, 64 * 1024},
	}
	if len(d.frames) != len(want) {
		t.Fatalf("frames %+v", d.frames)
	}
	for i := range want {
		if d.frames[i] != want[i] {
			t.Fatalf("frame %d = %+v, want %+v", i, d.frames[i], want[i])
		}
	}
}

func TestZeroSizeDevice(t *testing.T) {
	devs := []vmatest.Device{
		{ID: 1, Name: "drive-scsi0", Size: 0},
		vmatest.TPMState(2, 64*1024, 5),
	}
	h := testHeader(devs, 1, 1)
	stream := writeSequential(t, devs, h, testTrailer(h, 1))
	d, err := readAll(t, stream)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	checkContents(t, d, devs)
	if len(d.frames) != 1 || d.frames[0].Dev != 2 {
		t.Fatalf("frames %+v", d.frames)
	}
}

// --- determinism (dedup) ---

// body returns the stream up to (excluding) the trailer frame.
func body(t *testing.T, stream []byte) []byte {
	t.Helper()
	fs := splitFrames(t, stream)
	last := fs[len(fs)-1]
	if last.h.Type != TypeTrailer {
		t.Fatalf("last frame type %d", last.h.Type)
	}
	return stream[:len(stream)-len(last.raw)]
}

func TestDeterministicAcrossArrivalOrdersAndRuns(t *testing.T) {
	devs := testDevices()
	caps := reorder.Caps{RAMBytes: 64 << 20}
	hA := testHeader(devs, 0xaa, 1700000000) // different uuid/ctime per run
	hB := testHeader(devs, 0xbb, 1800000000)
	a, stA := writeViaBuffer(t, devs, hA, vmatest.SequentialOrder(devs), caps, 1)
	b, stB := writeViaBuffer(t, devs, hB, vmatest.ShuffledOrder(devs, 99), caps, 2)
	c, _ := writeViaBuffer(t, devs, hB, vmatest.InterleavedOrder(devs), caps, 3)
	if stA.PatchCount != 0 || stB.PatchCount != 0 {
		t.Fatal("determinism case must not overflow")
	}
	if !bytes.Equal(body(t, a), body(t, b)) || !bytes.Equal(body(t, a), body(t, c)) {
		t.Fatal("same contents and device table produced different bytes before the trailer")
	}
	if bytes.Equal(a, b) {
		t.Fatal("per-run trailer values did not reach the trailer")
	}
}

func TestHeaderFrameDeterministic(t *testing.T) {
	devs := testDevices()
	first := func(h vma.Header) []byte {
		var out bytes.Buffer
		w, err := NewWriter(&out, h)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Flush(); err != nil { // NewWriter buffers header and config frames
			t.Fatal(err)
		}
		fs := splitFrames(t, out.Bytes())
		if fs[0].h.Type != TypeHeader {
			t.Fatalf("first frame type %d", fs[0].h.Type)
		}
		return fs[0].raw
	}
	h1 := testHeader(devs, 0x01, 1)
	h2 := testHeader(devs, 0x02, 2)
	h2.Configs = h2.Configs[:1] // config changes do not touch the header frame
	if !bytes.Equal(first(h1), first(h2)) {
		t.Fatal("unchanged device table produced different header frames")
	}
	h3 := testHeader(devs, 0x01, 1)
	h3.Devices[1].Size += ClusterSize
	if bytes.Equal(first(h1), first(h3)) {
		t.Fatal("device size change not reflected in the header frame")
	}
	// payload layout
	p := first(h1)[FrameHeaderSize:]
	le := binary.LittleEndian
	if string(p[:4]) != "RVS1" || le.Uint32(p[4:]) != 1 || le.Uint32(p[8:]) != ClusterSize ||
		le.Uint32(p[12:]) != FrameBytes || le.Uint32(p[16:]) != 3 {
		t.Fatalf("header payload prefix % x", p[:20])
	}
	if p[20] != 1 || int(p[21]) != len("drive-efidisk0") || string(p[22:22+14]) != "drive-efidisk0" ||
		le.Uint64(p[36:]) != 256*1024 {
		t.Fatalf("first device entry % x", p[20:44])
	}
}

func TestOtherDevicesUnchangedByOneDeviceChange(t *testing.T) {
	devs := testDevices()
	h := testHeader(devs, 1, 1)
	a := writeSequential(t, devs, h, testTrailer(h, 1))
	changed := testDevices()
	changed[1].Data = append([]byte(nil), changed[1].Data...)
	changed[1].Data[2*ClusterSize+5] ^= 0xff
	b := writeSequential(t, changed, h, testTrailer(h, 1))
	fa, fb := splitFrames(t, a), splitFrames(t, b)
	if len(fa) != len(fb) {
		t.Fatal("frame count changed")
	}
	for i := range fa {
		same := bytes.Equal(fa[i].raw, fb[i].raw)
		if fa[i].h.Type == TypeData && fa[i].h.Dev == 2 {
			if same {
				t.Fatal("changed device frame identical")
			}
			continue
		}
		if !same {
			t.Fatalf("frame %d (%+v) changed although its device did not", i, fa[i].h)
		}
	}
}

// --- trailer ---

func TestTrailerCanonicalJSON(t *testing.T) {
	tr := Trailer{
		VMAUUID: "u", VMACTime: 5, Consistency: "crash", Tool: "vmastream <dev>",
		Devices: []TrailerDevice{{ID: 1, Name: "drive-scsi0", Size: 65536, SHA256CT: strings.Repeat("0", 64), AllocatedBytes: 4096, ZeroClusters: 0}},
	}
	b, err := MarshalTrailer(tr)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"buffer_ram_peak":0,"consistency":"crash",` +
		`"devices":[{"allocated_bytes":4096,"id":1,"name":"drive-scsi0","sha256ct":"` + strings.Repeat("0", 64) + `","size":65536,"zero_clusters":0}],` +
		`"early_clusters":0,"format":"RVS1","patch_count":0,"tool":"vmastream <dev>","vma_ctime":5,"vma_uuid":"u","vzdump_warnings":[]}`
	if string(b) != want {
		t.Fatalf("trailer JSON\n got %s\nwant %s", b, want)
	}
	again, _ := MarshalTrailer(tr)
	if !bytes.Equal(b, again) {
		t.Fatal("trailer encoding not stable")
	}
	var back Trailer
	if err := json.Unmarshal(b, &back); err != nil || back.Devices[0].Size != 65536 {
		t.Fatalf("unmarshal: %+v %v", back, err)
	}
}

func TestCanonicalJSONSortsNestedMapsAndKeepsLargeNumbers(t *testing.T) {
	b, err := CanonicalJSON(map[string]any{"z": 1, "a": map[string]any{"y": uint64(1<<63 + 5), "b": "<&>"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"a":{"b":"<&>","y":9223372036854775813},"z":1}` {
		t.Fatalf("got %s", b)
	}
}

// --- writer contract ---

func TestWriterRejectsContractViolations(t *testing.T) {
	devs := testDevices()
	h := testHeader(devs, 1, 1)
	newW := func() *Writer {
		w, err := NewWriter(io.Discard, h)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	if err := newW().InOrder(2, 0, nil); !errors.Is(err, ErrSequence) {
		t.Fatalf("InOrder out of cursor order: %v", err)
	}
	if err := newW().InOrder(1, 1, nil); !errors.Is(err, ErrSequence) {
		t.Fatalf("InOrder skipping a cluster: %v", err)
	}
	w := newW()
	_ = w.InOrder(1, 0, nil)
	_ = w.InOrder(1, 1, nil)
	if err := w.Patch(1, 1, make([]byte, ClusterSize)); !errors.Is(err, ErrSequence) {
		t.Fatalf("Patch behind the cursor: %v", err)
	}
	if err := newW().Patch(3, 0, make([]byte, ClusterSize)); !errors.Is(err, ErrSequence) {
		t.Fatalf("Patch of an unknown device: %v", err)
	}
	if err := newW().Patch(2, 1000, make([]byte, ClusterSize)); !errors.Is(err, ErrSequence) {
		t.Fatalf("Patch beyond the device: %v", err)
	}
	if err := newW().Patch(2, 3, make([]byte, 10)); !errors.Is(err, ErrSequence) {
		t.Fatalf("short Patch data: %v", err)
	}
	if err := newW().InOrder(1, 0, make([]byte, 10)); !errors.Is(err, ErrSequence) {
		t.Fatalf("short InOrder data: %v", err)
	}
	if err := newW().Close(testTrailer(h, 1)); !errors.Is(err, ErrSequence) {
		t.Fatalf("Close before every cluster: %v", err)
	}
	// trailer device table must match the header
	var out bytes.Buffer
	w2, _ := NewWriter(&out, h)
	for _, d := range devs {
		for i := uint64(0); i < vma.Clusters(d.Size); i++ {
			_ = w2.InOrder(d.ID, uint32(i), clusterOf(d, uint32(i)))
		}
	}
	bad := testTrailer(h, 1)
	bad.Devices[2].Size++
	if err := w2.Close(bad); !errors.Is(err, ErrSequence) {
		t.Fatalf("trailer device mismatch: %v", err)
	}
	// header validation
	hBad := testHeader(devs, 1, 1)
	hBad.Devices[0], hBad.Devices[1] = hBad.Devices[1], hBad.Devices[0]
	if _, err := NewWriter(io.Discard, hBad); err == nil {
		t.Fatal("unsorted device table accepted")
	}
}

type failWriter struct{ n int }

func (f *failWriter) Write(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, errors.New("disk full")
	}
	f.n--
	return len(p), nil
}

func TestWriterSurfacesOutputErrors(t *testing.T) {
	devs := testDevices()
	h := testHeader(devs, 1, 1)
	w, err := NewWriter(&failWriter{}, h)
	if err != nil {
		t.Fatal(err) // header is buffered
	}
	var got error
	for _, d := range devs {
		for i := uint64(0); i < vma.Clusters(d.Size) && got == nil; i++ {
			got = w.InOrder(d.ID, uint32(i), clusterOf(d, uint32(i)))
		}
	}
	if got == nil {
		got = w.Close(testTrailer(h, 1))
	}
	if got == nil || errors.Is(got, ErrIntegrity) || errors.Is(got, ErrSequence) {
		t.Fatalf("output error not surfaced as a plain write error: %v", got)
	}
}

// --- reader validation ---

func validStream(t *testing.T) ([]byte, []vmatest.Device) {
	devs := testDevices()
	h := testHeader(devs, 1, 1)
	order := vmatest.ShuffledOrder(devs, 3)
	caps := reorder.Caps{RAMBytes: reorder.BitmapBytes(layoutOf(h)) + 4096 + 2*ClusterSize}
	s, st := writeViaBuffer(t, devs, h, order, caps, 1)
	if st.PatchCount == 0 {
		t.Fatal("want patches in the validation fixture")
	}
	return s, devs
}

func findFrame(fs []rawFrame, typ FrameType, dev uint32) int {
	for i, f := range fs {
		if f.h.Type == typ && (dev == 0 || f.h.Dev == dev) {
			return i
		}
	}
	return -1
}

func TestReaderAcceptsValidStream(t *testing.T) {
	s, devs := validStream(t)
	d, err := readAll(t, s)
	if err != nil {
		t.Fatal(err)
	}
	checkContents(t, d, devs)
}

func TestReaderRejectsCorruptedHeaderCRC(t *testing.T) {
	s, _ := validStream(t)
	for _, which := range []string{"header", "config", "data", "patch", "trailer"} {
		fs := splitFrames(t, s)
		typ := map[string]FrameType{"header": TypeHeader, "config": TypeConfig, "data": TypeData, "patch": TypePatch, "trailer": TypeTrailer}[which]
		i := findFrame(fs, typ, 0)
		raw := append([]byte(nil), fs[i].raw...)
		raw[40] ^= 1 // reserved byte, covered only by the CRC
		fs[i].raw = raw
		_, err := readAll(t, joinFrames(fs))
		expectIntegrity(t, err, ErrCRC)
	}
}

func TestReaderRejectsOutOfOrderDataFrame(t *testing.T) {
	s, _ := validStream(t)
	fs := splitFrames(t, s)
	i, j := findFrame(fs, TypeData, 1), findFrame(fs, TypeData, 2)
	fs[i], fs[j] = fs[j], fs[i]
	_, err := readAll(t, joinFrames(fs))
	expectIntegrity(t, err, ErrOrder)

	// a data frame at an unexpected offset
	fs = splitFrames(t, s)
	i = findFrame(fs, TypeData, 2)
	h := fs[i].h
	h.Offset = ClusterSize
	h.Length -= ClusterSize
	fs[i] = reframe(h, fs[i].raw[FrameHeaderSize+ClusterSize:])
	_, err = readAll(t, joinFrames(fs))
	expectIntegrity(t, err, ErrOrder)

	// a data frame missing entirely: the trailer arrives too early
	fs = splitFrames(t, s)
	i = findFrame(fs, TypeData, 4)
	fs = append(fs[:i], fs[i+1:]...)
	_, err = readAll(t, joinFrames(fs))
	expectIntegrity(t, err, ErrOrder)

	// a patch frame after the data frame it patches
	fs = splitFrames(t, s)
	p := findFrame(fs, TypePatch, 0)
	pf := fs[p]
	fs = append(fs[:p], fs[p+1:]...)
	tr := findFrame(fs, TypeTrailer, 0)
	fs = append(fs[:tr], append([]rawFrame{pf}, fs[tr:]...)...)
	_, err = readAll(t, joinFrames(fs))
	expectIntegrity(t, err, ErrOrder)

	// config after the body started
	fs = splitFrames(t, s)
	c := findFrame(fs, TypeConfig, 0)
	cf := fs[c]
	fs = append(fs[:c], fs[c+1:]...)
	tr = findFrame(fs, TypeTrailer, 0)
	fs = append(fs[:tr], append([]rawFrame{cf}, fs[tr:]...)...)
	_, err = readAll(t, joinFrames(fs))
	expectIntegrity(t, err, ErrOrder)
}

func TestReaderRejectsWrongLength(t *testing.T) {
	s, _ := validStream(t)
	fs := splitFrames(t, s)
	i := findFrame(fs, TypeData, 2)
	h := fs[i].h
	h.Length--
	fs[i] = reframe(h, fs[i].raw[FrameHeaderSize:FrameHeaderSize+int(h.Length)])
	_, err := readAll(t, joinFrames(fs))
	expectIntegrity(t, err, ErrLength)

	// a patch frame whose length is not whole clusters (and not the device end)
	fs = splitFrames(t, s)
	p := findFrame(fs, TypePatch, 0)
	h = fs[p].h
	if h.Offset+h.Length == 4<<20+1000 {
		t.Skip("fixture patch sits at the device end")
	}
	h.Length = 100
	fs[p] = reframe(h, fs[p].raw[FrameHeaderSize:FrameHeaderSize+100])
	_, err = readAll(t, joinFrames(fs))
	expectIntegrity(t, err, ErrLength)
}

func TestReaderRejectsBadOffsetsAndDevices(t *testing.T) {
	s, _ := validStream(t)
	mutate := func(f func(h *FrameHeader)) error {
		fs := splitFrames(t, s)
		p := findFrame(fs, TypePatch, 0)
		h := fs[p].h
		f(&h)
		fs[p] = reframe(h, fs[p].raw[FrameHeaderSize:])
		_, err := readAll(t, joinFrames(fs))
		return err
	}
	expectIntegrity(t, mutate(func(h *FrameHeader) { h.Offset++ }), ErrOffset)
	expectIntegrity(t, mutate(func(h *FrameHeader) { h.Offset = 1 << 40 }), ErrOffset)
	expectIntegrity(t, mutate(func(h *FrameHeader) { h.Dev = 3 }), ErrOffset)
	expectIntegrity(t, mutate(func(h *FrameHeader) { h.Type = 9 }), ErrFrameType)
	expectIntegrity(t, mutate(func(h *FrameHeader) { *h = FrameHeader{Type: TypeHeader, Length: h.Length} }), ErrOrder)
}

func TestReaderRejectsTruncationAndTrailingData(t *testing.T) {
	s, _ := validStream(t)
	fs := splitFrames(t, s)
	tr := findFrame(fs, TypeTrailer, 0)
	d := findFrame(fs, TypeData, 2)
	start := len(joinFrames(fs[:d]))
	for _, cut := range []int{
		10,                          // inside the header frame header
		start + FrameHeaderSize/2,   // inside a frame header
		start + FrameHeaderSize + 5, // inside a data payload
		len(joinFrames(fs[:tr])),    // trailer missing
		len(s) - 3,                  // inside the trailer
	} {
		_, err := readAll(t, s[:cut])
		expectIntegrity(t, err, ErrTruncated)
	}
	_, err := readAll(t, append(append([]byte(nil), s...), 0))
	expectIntegrity(t, err, ErrTrailingData)
}

func TestReaderRejectsBadTrailerAndHeader(t *testing.T) {
	s, _ := validStream(t)
	fs := splitFrames(t, s)
	i := findFrame(fs, TypeTrailer, 0)
	var tr Trailer
	if err := json.Unmarshal(fs[i].raw[FrameHeaderSize:], &tr); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Trailer){
		"device size":   func(t *Trailer) { t.Devices[0].Size++ },
		"device name":   func(t *Trailer) { t.Devices[1].Name = "drive-scsi9" },
		"device count":  func(t *Trailer) { t.Devices = t.Devices[:2] },
		"digest format": func(t *Trailer) { t.Devices[0].SHA256CT = "xyz" },
	} {
		c := tr
		c.Devices = append([]TrailerDevice(nil), tr.Devices...)
		mut(&c)
		j, _ := json.Marshal(c)
		g := append([]rawFrame(nil), fs...)
		g[i] = reframe(FrameHeader{Type: TypeTrailer, Length: uint64(len(j))}, j)
		_, err := readAll(t, joinFrames(g))
		if !errors.Is(err, ErrIntegrity) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	g := append([]rawFrame(nil), fs...)
	g[i] = reframe(FrameHeader{Type: TypeTrailer, Length: 5}, []byte("{nope"))
	_, err := readAll(t, joinFrames(g))
	expectIntegrity(t, err, ErrPayload)

	// header payload with a wrong version
	g = append([]rawFrame(nil), fs...)
	p := append([]byte(nil), g[0].raw[FrameHeaderSize:]...)
	p[4] = 2
	g[0] = reframe(g[0].h, p)
	_, err = readAll(t, joinFrames(g))
	expectIntegrity(t, err, ErrPayload)

	// stream not starting with a header frame
	_, err = readAll(t, joinFrames(fs[1:]))
	expectIntegrity(t, err, ErrOrder)

	// bad magic
	bad := append([]byte(nil), s...)
	bad[0] = 'X'
	_, err = readAll(t, bad)
	expectIntegrity(t, err, ErrMagic)
}

func TestReaderTrailerBeforeEnd(t *testing.T) {
	s, _ := validStream(t)
	r, err := NewReader(bytes.NewReader(s))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Trailer(); err == nil {
		t.Fatal("Trailer available before the end")
	}
}
