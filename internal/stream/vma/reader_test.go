package vma_test

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/norandom/restic-pve/internal/stream/vma"
	"github.com/norandom/restic-pve/internal/stream/vma/vmatest"
)

func readerDevices() []vmatest.Device {
	return []vmatest.Device{
		vmatest.DataDisk(1, "scsi0", 300*vma.ClusterSize+12345, 1),
		vmatest.EFIDisk(2, 8*vma.ClusterSize, 2),
		vmatest.TPMState(3, vma.ClusterSize, 3),
	}
}

func readerBuilder(tm vmatest.Tamper) *vmatest.Builder {
	devs := readerDevices()
	return &vmatest.Builder{
		UUID:    fixedUUID,
		CTime:   fixedCTime,
		Devices: devs,
		Configs: []vma.ConfigBlob{
			vmatest.ExampleConfig(),
			{Name: "qemu-server.fw", Data: []byte("[OPTIONS]\nenable: 1\n")},
		},
		Order:  vmatest.InterleavedOrder(devs),
		Tamper: tm,
	}
}

func build(t *testing.T, b *vmatest.Builder) *vmatest.Result {
	t.Helper()
	r, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return r
}

// onlyReader hides any other interface (e.g. io.Seeker, io.WriterTo) of the source.
type onlyReader struct{ r io.Reader }

func (o onlyReader) Read(p []byte) (int, error) { return o.r.Read(p) }

// smallReads returns at most n bytes per Read, to exercise partial reads.
type smallReads struct {
	r io.Reader
	n int
}

func (s *smallReads) Read(p []byte) (int, error) {
	if len(p) > s.n {
		p = p[:s.n]
	}
	return s.r.Read(p)
}

// readAll parses the whole stream and reassembles each device (truncated to its size).
// It returns the first error that is not io.EOF.
func readAll(stream []byte) (*vma.Header, map[uint8][]byte, error) {
	r, err := vma.NewReader(onlyReader{bytes.NewReader(stream)})
	if err != nil {
		return nil, nil, err
	}
	h := r.Header()
	out := map[uint8][]byte{}
	for _, d := range h.Devices {
		out[d.ID] = make([]byte, vma.Clusters(d.Size)*vma.ClusterSize)
	}
	for {
		c, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return h, out, err
		}
		if c.Zero {
			if c.Data != nil {
				return h, out, errors.New("zero cluster with data")
			}
			continue
		}
		if len(c.Data) != vma.ClusterSize {
			return h, out, errors.New("cluster data is not ClusterSize bytes")
		}
		copy(out[c.Dev][uint64(c.Index)*vma.ClusterSize:], c.Data)
	}
	for _, d := range h.Devices {
		out[d.ID] = out[d.ID][:d.Size]
	}
	return h, out, nil
}

func wantFormatError(t *testing.T, err error, kind vma.ErrorKind, off int64) {
	t.Helper()
	var fe *vma.FormatError
	if !errors.As(err, &fe) {
		t.Fatalf("error %v (%T), want *vma.FormatError kind %v", err, err, kind)
	}
	if fe.Kind != kind || fe.Offset != off {
		t.Fatalf("got kind %v offset %d (%v), want kind %v offset %d", fe.Kind, fe.Offset, fe, kind, off)
	}
}

func TestReaderParsesValidMultiDeviceStream(t *testing.T) {
	b := readerBuilder(vmatest.Tamper{})
	res := build(t, b)
	h, got, err := readAll(res.Stream)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if h.UUID != fixedUUID || h.CTime != fixedCTime {
		t.Errorf("uuid %x ctime %d", h.UUID, h.CTime)
	}
	wantDevs := []vma.DevInfo{
		{ID: 1, Name: "drive-scsi0", Size: 300*vma.ClusterSize + 12345},
		{ID: 2, Name: "drive-efidisk0", Size: 8 * vma.ClusterSize},
		{ID: 3, Name: "drive-tpmstate0-backup", Size: vma.ClusterSize},
	}
	if len(h.Devices) != len(wantDevs) {
		t.Fatalf("devices %+v", h.Devices)
	}
	for i, d := range wantDevs {
		if h.Devices[i] != d {
			t.Errorf("device %d: %+v, want %+v", i, h.Devices[i], d)
		}
	}
	if len(h.Configs) != len(b.Configs) {
		t.Fatalf("configs %d, want %d", len(h.Configs), len(b.Configs))
	}
	for i, c := range b.Configs {
		if h.Configs[i].Name != c.Name || !bytes.Equal(h.Configs[i].Data, c.Data) {
			t.Errorf("config %d: %q differs", i, h.Configs[i].Name)
		}
	}
	for _, d := range b.Devices {
		if !bytes.Equal(got[d.ID], b.Contents(d.ID)) {
			t.Errorf("device %d contents differ", d.ID)
		}
	}
}

func TestReaderClusterStream(t *testing.T) {
	// One small device: check every cluster is delivered once, with Zero and
	// mask-expanded data, in stream order, and that Next keeps returning io.EOF.
	dev := vmatest.DataDisk(1, "virtio0", 10*vma.ClusterSize, 7)
	b := &vmatest.Builder{UUID: fixedUUID, CTime: fixedCTime, Devices: []vmatest.Device{dev},
		Order: vmatest.ShuffledOrder([]vmatest.Device{dev}, 3)}
	res := build(t, b)
	r, err := vma.NewReader(&smallReads{r: bytes.NewReader(res.Stream), n: 1000})
	if err != nil {
		t.Fatal(err)
	}
	contents := b.Contents(1)
	var order []uint32
	for {
		c, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if c.Dev != 1 {
			t.Fatalf("dev %d", c.Dev)
		}
		want := contents[uint64(c.Index)*vma.ClusterSize:][:vma.ClusterSize]
		zero := bytes.Equal(want, make([]byte, vma.ClusterSize))
		if c.Zero != zero {
			t.Errorf("cluster %d: Zero=%v want %v", c.Index, c.Zero, zero)
		}
		if !zero && !bytes.Equal(c.Data, want) {
			t.Errorf("cluster %d: data differs", c.Index)
		}
		order = append(order, c.Index)
	}
	for i, ref := range b.Order {
		if order[i] != ref.Index {
			t.Fatalf("order %v, want builder order", order)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := r.Next(); err != io.EOF {
			t.Fatalf("after EOF: %v", err)
		}
	}
}

func TestReaderAccepts59ClusterExtent(t *testing.T) {
	// Rebuild a 59-cluster device as a single extent of 59 entries.
	dev := vmatest.DataDisk(1, "sata0", vma.MaxExtentClusters*vma.ClusterSize, 9)
	b := &vmatest.Builder{UUID: fixedUUID, CTime: fixedCTime, Devices: []vmatest.Device{dev}}
	res := build(t, b)
	l := inspect(t, res.Stream)
	if len(l.Extents) != 2 {
		t.Fatalf("extents %d", len(l.Extents))
	}
	e0, e1 := l.Extents[0], l.Extents[1]
	h := append([]byte(nil), res.Stream[e0.Offset:e0.Offset+vma.ExtentHeaderSize]...)
	be := binary.BigEndian
	info := be.Uint64(res.Stream[e1.Offset+vma.OffExtBlockInfo+8*int64(e1.Entries[0].Slot):])
	be.PutUint64(h[vma.OffExtBlockInfo+8*58:], info)
	be.PutUint16(h[vma.OffExtBlockCount:], e0.BlockCount+e1.BlockCount)
	resealExtent(h)
	var s []byte
	s = append(s, res.Stream[:e0.Offset]...)
	s = append(s, h...)
	s = append(s, res.Stream[e0.Offset+vma.ExtentHeaderSize:e1.Offset]...)
	s = append(s, res.Stream[e1.Offset+vma.ExtentHeaderSize:]...)
	_, got, err := readAll(s)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got[1], b.Contents(1)) {
		t.Error("contents differ")
	}
}

func TestReaderRejectsTamperedStreams(t *testing.T) {
	cases := []struct {
		name string
		tm   vmatest.Tamper
		kind vma.ErrorKind
	}{
		{"header md5", vmatest.Tamper{Kind: vmatest.TamperHeaderMD5}, vma.ErrHeaderMD5},
		{"extent md5", vmatest.Tamper{Kind: vmatest.TamperExtentMD5, Extent: 2}, vma.ErrExtentMD5},
		{"extent uuid", vmatest.Tamper{Kind: vmatest.TamperExtentUUID, Extent: 1}, vma.ErrUUID},
		{"reserved", vmatest.Tamper{Kind: vmatest.TamperReserved, Extent: 3}, vma.ErrReserved},
		{"blockinfo device", vmatest.Tamper{Kind: vmatest.TamperBlockInfoDevice, Extent: 1}, vma.ErrBlockInfo},
		{"blockinfo range", vmatest.Tamper{Kind: vmatest.TamperBlockInfoRange, Extent: 2}, vma.ErrBlockInfo},
		{"blockinfo reserved", vmatest.Tamper{Kind: vmatest.TamperBlockInfoReserved, Extent: 0}, vma.ErrBlockInfo},
		{"duplicate cluster", vmatest.Tamper{Kind: vmatest.TamperDuplicateCluster, Extent: 1}, vma.ErrDuplicate},
		{"missing cluster", vmatest.Tamper{Kind: vmatest.TamperMissingCluster, Extent: 2}, vma.ErrMissingCluster},
		{"missing final extent", vmatest.Tamper{Kind: vmatest.TamperMissingCluster, Extent: 5}, vma.ErrMissingCluster},
		{"truncated mid-extent data", vmatest.Tamper{Kind: vmatest.TamperTruncate}, vma.ErrTruncated},
		{"truncated mid-extent header", vmatest.Tamper{Kind: vmatest.TamperTruncate, At: 12288 + 512 + 100}, vma.ErrTruncated},
		{"truncated in header", vmatest.Tamper{Kind: vmatest.TamperTruncate, At: 5000}, vma.ErrTruncated},
		{"truncated in blob buffer", vmatest.Tamper{Kind: vmatest.TamperTruncate, At: vma.HeaderFixedSize + 10}, vma.ErrTruncated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := build(t, readerBuilder(tc.tm))
			_, _, err := readAll(res.Stream)
			wantFormatError(t, err, tc.kind, res.TamperOffset)
		})
	}
}

func TestReaderTruncatedAtExtentBoundaryIsMissingCluster(t *testing.T) {
	res := build(t, readerBuilder(vmatest.Tamper{}))
	cut := res.ExtentOffsets[len(res.ExtentOffsets)-1]
	_, _, err := readAll(res.Stream[:cut])
	wantFormatError(t, err, vma.ErrMissingCluster, cut)
}

func TestReaderErrorIsSticky(t *testing.T) {
	res := build(t, readerBuilder(vmatest.Tamper{Kind: vmatest.TamperExtentMD5, Extent: 1}))
	r, err := vma.NewReader(bytes.NewReader(res.Stream))
	if err != nil {
		t.Fatal(err)
	}
	var first error
	for first == nil {
		_, first = r.Next()
	}
	_, again := r.Next()
	wantFormatError(t, first, vma.ErrExtentMD5, res.TamperOffset)
	wantFormatError(t, again, vma.ErrExtentMD5, res.TamperOffset)
}

func TestReaderForbiddenDevices(t *testing.T) {
	allowed := []string{"drive-scsi0", "drive-scsi12", "drive-virtio3", "drive-sata1", "drive-ide2",
		"drive-efidisk0", "drive-tpmstate0-backup", "drive-tpmstate1-backup"}
	forbidden := []string{"vmstate", "drive-scsi", "drive-tpmstate0", "drive-nvme0", "scsi0",
		"drive-scsi0x", "drive-efidisk", "Drive-scsi0", "drive-scsi-1"}
	for _, name := range allowed {
		b := &vmatest.Builder{UUID: fixedUUID, CTime: fixedCTime,
			Devices: []vmatest.Device{{ID: 1, Name: name, Size: vma.ClusterSize}}}
		if _, _, err := readAll(build(t, b).Stream); err != nil {
			t.Errorf("%q rejected: %v", name, err)
		}
	}
	for _, name := range forbidden {
		b := &vmatest.Builder{UUID: fixedUUID, CTime: fixedCTime,
			Devices: []vmatest.Device{vmatest.DataDisk(1, "scsi0", vma.ClusterSize, 1), {ID: 4, Name: name, Size: vma.ClusterSize}}}
		_, _, err := readAll(build(t, b).Stream)
		wantFormatError(t, err, vma.ErrForbiddenDevice, vma.OffDevInfo+4*vma.DevInfoSize)
	}
	b := &vmatest.Builder{UUID: fixedUUID, CTime: fixedCTime,
		Devices: []vmatest.Device{vmatest.DataDisk(1, "scsi0", vma.ClusterSize, 1), vmatest.VMStateDevice(2, 4*vma.ClusterSize)}}
	_, _, err := readAll(build(t, b).Stream)
	wantFormatError(t, err, vma.ErrForbiddenDevice, vma.OffDevInfo+2*vma.DevInfoSize)
}

// resealHeader recomputes the header md5 of a stream.
func resealHeader(s []byte) {
	n := binary.BigEndian.Uint32(s[vma.OffHeaderSize:])
	clear(s[vma.OffMD5 : vma.OffMD5+16])
	sum := md5.Sum(s[:n])
	copy(s[vma.OffMD5:], sum[:])
}

func resealExtent(h []byte) {
	clear(h[vma.OffExtMD5 : vma.OffExtMD5+16])
	sum := md5.Sum(h)
	copy(h[vma.OffExtMD5:], sum[:])
}

func TestReaderRejectsMalformedHeaders(t *testing.T) {
	be := binary.BigEndian
	cases := []struct {
		name   string
		mutate func(s []byte)
		reseal bool
		kind   vma.ErrorKind
		off    int64
	}{
		{"magic", func(s []byte) { s[3] = 'X' }, true, vma.ErrMagic, 0},
		{"version", func(s []byte) { be.PutUint32(s[vma.OffVersion:], 2) }, true, vma.ErrVersion, vma.OffVersion},
		{"blob buffer md5", func(s []byte) { s[vma.HeaderFixedSize+5] ^= 1 }, false, vma.ErrHeaderMD5, 0},
		{"dev_info md5", func(s []byte) { s[vma.OffDevInfo+vma.DevInfoSize+15] ^= 1 }, false, vma.ErrHeaderMD5, 0},
		{"header size too small", func(s []byte) { be.PutUint32(s[vma.OffHeaderSize:], 4096) }, true, vma.ErrHeader, vma.OffHeaderSize},
		{"blob buffer beyond header", func(s []byte) { be.PutUint32(s[vma.OffBlobBufferSize:], 1<<20) }, true, vma.ErrHeader, vma.OffBlobBufferSize},
		{"blob buffer offset", func(s []byte) { be.PutUint32(s[vma.OffBlobBufferOffset:], 100) }, true, vma.ErrHeader, vma.OffBlobBufferOffset},
		{"config name pointer", func(s []byte) { be.PutUint32(s[vma.OffConfigNames:], 60000) }, true, vma.ErrHeader, vma.OffConfigNames},
		{"config data without name", func(s []byte) { be.PutUint32(s[vma.OffConfigNames:], 0) }, true, vma.ErrHeader, vma.OffConfigNames},
		{"blob length", func(s []byte) {
			p := be.Uint32(s[vma.OffConfigData:])
			binary.LittleEndian.PutUint16(s[vma.HeaderFixedSize+int(p):], 0xFFFF)
		}, true, vma.ErrHeader, vma.OffConfigData},
		{"device name not NUL-terminated", func(s []byte) {
			p := be.Uint32(s[vma.OffDevInfo+vma.DevInfoSize:])
			n := binary.LittleEndian.Uint16(s[vma.HeaderFixedSize+int(p):])
			s[vma.HeaderFixedSize+int(p)+2+int(n)-1] = 'x'
		}, true, vma.ErrHeader, vma.OffDevInfo + vma.DevInfoSize},
		{"device without name", func(s []byte) { be.PutUint32(s[vma.OffDevInfo+vma.DevInfoSize:], 0) }, true, vma.ErrHeader, vma.OffDevInfo + vma.DevInfoSize},
		{"device 0", func(s []byte) {
			copy(s[vma.OffDevInfo:vma.OffDevInfo+vma.DevInfoSize], s[vma.OffDevInfo+vma.DevInfoSize:])
		}, true, vma.ErrHeader, vma.OffDevInfo},
		{"duplicate device name", func(s []byte) {
			copy(s[vma.OffDevInfo+2*vma.DevInfoSize:vma.OffDevInfo+3*vma.DevInfoSize], s[vma.OffDevInfo+vma.DevInfoSize:])
		}, true, vma.ErrHeader, vma.OffDevInfo + 2*vma.DevInfoSize},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := build(t, readerBuilder(vmatest.Tamper{}))
			s := res.Stream
			tc.mutate(s)
			if tc.reseal {
				resealHeader(s)
			}
			_, _, err := readAll(s)
			wantFormatError(t, err, tc.kind, tc.off)
		})
	}
}

func TestReaderRejectsMalformedExtents(t *testing.T) {
	be := binary.BigEndian
	cases := []struct {
		name   string
		mutate func(h []byte)
		kind   vma.ErrorKind
	}{
		{"magic", func(h []byte) { h[3] = 'X' }, vma.ErrMagic},
		{"block count", func(h []byte) { be.PutUint16(h[vma.OffExtBlockCount:], be.Uint16(h[vma.OffExtBlockCount:])+1) }, vma.ErrBlockInfo},
		{"device 0", func(h []byte) {
			p := vma.OffExtBlockInfo + 8*57
			be.PutUint64(h[p:], 5) // dev 0, cluster 5
		}, vma.ErrBlockInfo},
		{"duplicate within extent", func(h []byte) {
			// Copy the entry of slot 0 into slot 1 and keep the block count consistent
			// (all masks are full here, so the data length is unchanged).
			first := be.Uint64(h[vma.OffExtBlockInfo:])
			second := be.Uint64(h[vma.OffExtBlockInfo+8:])
			be.PutUint64(h[vma.OffExtBlockInfo+8:], first)
			bc := int(be.Uint16(h[vma.OffExtBlockCount:])) - popcount(uint16(second>>48)) + popcount(uint16(first>>48))
			be.PutUint16(h[vma.OffExtBlockCount:], uint16(bc))
		}, vma.ErrDuplicate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// One device in plain order, all clusters non-zero, so block counts stay simple.
			dev := vmatest.Device{ID: 1, Name: "drive-scsi0", Size: 120 * vma.ClusterSize, Data: bytes.Repeat([]byte{7}, 120*vma.ClusterSize)}
			res := build(t, &vmatest.Builder{UUID: fixedUUID, CTime: fixedCTime, Devices: []vmatest.Device{dev}})
			off := res.ExtentOffsets[1]
			s := res.Stream
			h := s[off : off+vma.ExtentHeaderSize]
			tc.mutate(h)
			resealExtent(h)
			_, _, err := readAll(s)
			wantFormatError(t, err, tc.kind, off)
		})
	}
}

func popcount(m uint16) int {
	n := 0
	for ; m != 0; m &= m - 1 {
		n++
	}
	return n
}

type errAfter struct {
	r   io.Reader
	n   int
	err error
}

func (e *errAfter) Read(p []byte) (int, error) {
	if e.n <= 0 {
		return 0, e.err
	}
	if len(p) > e.n {
		p = p[:e.n]
	}
	n, err := e.r.Read(p)
	e.n -= n
	return n, err
}

func TestReaderPropagatesReadErrors(t *testing.T) {
	res := build(t, readerBuilder(vmatest.Tamper{}))
	boom := errors.New("boom")
	r, err := vma.NewReader(&errAfter{r: bytes.NewReader(res.Stream), n: int(res.ExtentOffsets[1]) + 100, err: boom})
	if err != nil {
		t.Fatal(err)
	}
	for err == nil {
		_, err = r.Next()
	}
	var fe *vma.FormatError
	if !errors.Is(err, boom) || errors.As(err, &fe) {
		t.Fatalf("err %v, want wrapped read error", err)
	}
}
