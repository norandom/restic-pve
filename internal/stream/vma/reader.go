package vma

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"io"
	"regexp"
)

// ErrorKind classifies a structural defect of a VMA stream.
type ErrorKind int

const (
	// ErrMagic: header or extent magic is wrong.
	ErrMagic ErrorKind = iota + 1
	// ErrVersion: unsupported format version.
	ErrVersion
	// ErrHeader: malformed header layout (sizes, blob pointers/lengths, dev_info, config slots).
	ErrHeader
	// ErrHeaderMD5: header md5 mismatch.
	ErrHeaderMD5
	// ErrExtentMD5: extent header md5 mismatch.
	ErrExtentMD5
	// ErrUUID: extent uuid differs from the header uuid.
	ErrUUID
	// ErrReserved: extent reserved field is not zero.
	ErrReserved
	// ErrBlockInfo: malformed blockinfo (reserved bits, unknown device,
	// cluster out of range, block count not matching the masks).
	ErrBlockInfo
	// ErrTruncated: the stream ends inside the header or an extent.
	ErrTruncated
	// ErrDuplicate: a cluster arrives more than once.
	ErrDuplicate
	// ErrMissingCluster: at the end of the stream some cluster never arrived.
	ErrMissingCluster
	// ErrForbiddenDevice: a device name outside the allow pattern (e.g. "vmstate").
	ErrForbiddenDevice
)

var kindNames = map[ErrorKind]string{
	ErrMagic:           "bad magic",
	ErrVersion:         "unsupported version",
	ErrHeader:          "malformed header",
	ErrHeaderMD5:       "header md5 mismatch",
	ErrExtentMD5:       "extent md5 mismatch",
	ErrUUID:            "extent uuid mismatch",
	ErrReserved:        "extent reserved field not zero",
	ErrBlockInfo:       "bad blockinfo",
	ErrTruncated:       "truncated stream",
	ErrDuplicate:       "duplicate cluster",
	ErrMissingCluster:  "missing cluster",
	ErrForbiddenDevice: "forbidden device",
}

func (k ErrorKind) String() string {
	if s, ok := kindNames[k]; ok {
		return s
	}
	return fmt.Sprintf("ErrorKind(%d)", int(k))
}

// FormatError reports a structural defect at a stream offset: 0 for the
// header md5, the field offset for other header defects, the extent start for
// extent defects, the end of the data read for truncation and missing clusters.
type FormatError struct {
	Kind   ErrorKind
	Offset int64
	Detail string
}

func (e *FormatError) Error() string {
	return fmt.Sprintf("vma: %s at offset %d: %s", e.Kind, e.Offset, e.Detail)
}

func formatErr(kind ErrorKind, off int64, format string, args ...any) *FormatError {
	return &FormatError{Kind: kind, Offset: off, Detail: fmt.Sprintf(format, args...)}
}

// Cluster is one cluster as it arrives in the stream.
type Cluster struct {
	Dev   uint8
	Index uint32 // cluster number within the device
	Zero  bool   // mask == 0
	Data  []byte // ClusterSize bytes expanded from the 4 KiB blocks (nil when Zero)
}

// allowedDevice is the device name allow pattern; anything else (notably the
// memory state device "vmstate") is forbidden.
var allowedDevice = regexp.MustCompile(`^drive-((scsi|virtio|sata|ide|efidisk)[0-9]+|tpmstate[0-9]+-backup)$`)

// maxHeaderSize bounds header_size: the fixed part plus a blob buffer that
// holds every config name and data blob and every device name at full size.
const maxHeaderSize = HeaderFixedSize + (2*MaxConfigs+256)*(2+MaxBlobSize) + 512

type readDev struct {
	clusters uint64
	seen     []uint64 // bitmap of arrived clusters
	count    uint64
}

type pendingEntry struct {
	mask  uint16
	dev   uint8
	index uint32
}

// Reader reads a VMA v1 stream from a plain io.Reader (no seeking). Memory use
// is the header, one bit per device cluster, and one cluster at a time: an
// extent's data is read cluster by cluster in Next.
type Reader struct {
	r    io.Reader
	pos  int64 // bytes consumed from r
	h    Header
	devs [256]*readDev
	err  error // sticky

	entries []pendingEntry // entries of the current extent not yet returned
	ext     [ExtentHeaderSize]byte
}

// NewReader reads and verifies the header.
func NewReader(r io.Reader) (*Reader, error) {
	vr := &Reader{r: r}
	if err := vr.readHeader(); err != nil {
		return nil, err
	}
	return vr, nil
}

// Header returns the decoded header.
func (r *Reader) Header() *Header { return &r.h }

// readFull reads len(b) bytes. A short read at EOF is reported as
// io.ErrUnexpectedEOF (or io.EOF when nothing was read); other read errors are wrapped.
func (r *Reader) readFull(b []byte) error {
	n, err := io.ReadFull(r.r, b)
	r.pos += int64(n)
	switch {
	case err == nil, err == io.EOF, err == io.ErrUnexpectedEOF:
		return err
	default:
		return fmt.Errorf("vma: read at offset %d: %w", r.pos, err)
	}
}

func (r *Reader) truncated(what string, err error) error {
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return formatErr(ErrTruncated, r.pos, "stream ends inside %s", what)
	}
	return err
}

func (r *Reader) readHeader() error {
	be := binary.BigEndian
	fixed := make([]byte, HeaderFixedSize)
	if err := r.readFull(fixed); err != nil {
		return r.truncated("the header", err)
	}
	if !bytes.Equal(fixed[OffMagic:OffMagic+4], HeaderMagic[:]) {
		return formatErr(ErrMagic, OffMagic, "header magic %q", fixed[OffMagic:OffMagic+4])
	}
	if v := be.Uint32(fixed[OffVersion:]); v != Version {
		return formatErr(ErrVersion, OffVersion, "version %d, want %d", v, Version)
	}
	size := be.Uint32(fixed[OffHeaderSize:])
	if size < HeaderFixedSize || size > maxHeaderSize {
		return formatErr(ErrHeader, OffHeaderSize, "header_size %d", size)
	}
	bufOff := be.Uint32(fixed[OffBlobBufferOffset:])
	bufSize := be.Uint32(fixed[OffBlobBufferSize:])
	if bufOff < HeaderFixedSize || bufOff > size {
		return formatErr(ErrHeader, OffBlobBufferOffset, "blob_buffer_offset %d outside %d..%d", bufOff, HeaderFixedSize, size)
	}
	if uint64(bufOff)+uint64(bufSize) > uint64(size) {
		return formatErr(ErrHeader, OffBlobBufferSize, "blob buffer %d+%d exceeds header_size %d", bufOff, bufSize, size)
	}
	head := make([]byte, size)
	copy(head, fixed)
	if err := r.readFull(head[HeaderFixedSize:]); err != nil {
		return r.truncated("the header", err)
	}
	var stored [16]byte
	copy(stored[:], head[OffMD5:])
	clear(head[OffMD5 : OffMD5+16])
	if sum := md5.Sum(head); sum != stored {
		return formatErr(ErrHeaderMD5, 0, "stored %x, computed %x", stored, sum)
	}
	copy(r.h.UUID[:], head[OffUUID:])
	r.h.CTime = int64(be.Uint64(head[OffCTime:]))

	blobs := head[bufOff : bufOff+bufSize]
	blob := func(ptr uint32) ([]byte, error) {
		if ptr == 0 || uint64(ptr)+2 > uint64(len(blobs)) {
			return nil, fmt.Errorf("blob pointer %d outside the blob buffer (%d bytes)", ptr, len(blobs))
		}
		n := uint64(binary.LittleEndian.Uint16(blobs[ptr:]))
		if uint64(ptr)+2+n > uint64(len(blobs)) {
			return nil, fmt.Errorf("blob at %d (%d bytes) exceeds the blob buffer (%d bytes)", ptr, n, len(blobs))
		}
		return blobs[uint64(ptr)+2 : uint64(ptr)+2+n], nil
	}
	str := func(ptr uint32) (string, error) {
		d, err := blob(ptr)
		if err != nil {
			return "", err
		}
		if len(d) < 2 || d[len(d)-1] != 0 || bytes.IndexByte(d[:len(d)-1], 0) >= 0 {
			return "", fmt.Errorf("string blob at %d is not a non-empty NUL-terminated string", ptr)
		}
		return string(d[:len(d)-1]), nil
	}

	for i := 0; i < MaxConfigs; i++ {
		nameOff := int64(OffConfigNames + 4*i)
		dataOff := int64(OffConfigData + 4*i)
		np, dp := be.Uint32(head[nameOff:]), be.Uint32(head[dataOff:])
		if np == 0 && dp == 0 {
			continue
		}
		if np == 0 || dp == 0 {
			return formatErr(ErrHeader, nameOff, "config %d: name pointer %d, data pointer %d", i, np, dp)
		}
		name, err := str(np)
		if err != nil {
			return formatErr(ErrHeader, nameOff, "config %d name: %v", i, err)
		}
		data, err := blob(dp)
		if err != nil {
			return formatErr(ErrHeader, dataOff, "config %d data: %v", i, err)
		}
		r.h.Configs = append(r.h.Configs, ConfigBlob{Name: name, Data: append([]byte(nil), data...)})
	}

	names := map[string]bool{}
	for id := 0; id < 256; id++ {
		off := int64(OffDevInfo + id*DevInfoSize)
		ptr, devSize := be.Uint32(head[off:]), be.Uint64(head[off+8:])
		if ptr == 0 && devSize == 0 {
			continue
		}
		switch {
		case id < MinDevID || id > MaxDevID:
			return formatErr(ErrHeader, off, "device id %d is reserved", id)
		case ptr == 0 || devSize == 0:
			return formatErr(ErrHeader, off, "device %d: name pointer %d, size %d", id, ptr, devSize)
		case Clusters(devSize) > 1<<32:
			return formatErr(ErrHeader, off, "device %d: size %d too large", id, devSize)
		}
		name, err := str(ptr)
		if err != nil {
			return formatErr(ErrHeader, off, "device %d name: %v", id, err)
		}
		if names[name] {
			return formatErr(ErrHeader, off, "device %d: duplicate name %q", id, name)
		}
		names[name] = true
		if !allowedDevice.MatchString(name) {
			return formatErr(ErrForbiddenDevice, off, "device %d: name %q is not an allowed disk device", id, name)
		}
		n := Clusters(devSize)
		r.devs[id] = &readDev{clusters: n, seen: make([]uint64, (n+63)/64)}
		r.h.Devices = append(r.h.Devices, DevInfo{ID: uint8(id), Name: name, Size: devSize})
	}
	return nil
}

// Next returns the next cluster in stream order. It verifies each extent
// header before returning any of its clusters. After the last extent it checks
// that every cluster of every device arrived exactly once and returns io.EOF.
// Any error is sticky.
func (r *Reader) Next() (Cluster, error) {
	if r.err != nil {
		return Cluster{}, r.err
	}
	c, err := r.next()
	if err != nil {
		r.err = err
	}
	return c, err
}

func (r *Reader) next() (Cluster, error) {
	for len(r.entries) == 0 {
		if err := r.readExtent(); err != nil {
			return Cluster{}, err
		}
	}
	e := r.entries[0]
	r.entries = r.entries[1:]
	c := Cluster{Dev: e.dev, Index: e.index}
	if e.mask == 0 {
		c.Zero = true
		return c, nil
	}
	c.Data = make([]byte, ClusterSize)
	for i := 0; i < BlocksPerCluster; i++ {
		if e.mask&(1<<i) == 0 {
			continue
		}
		if err := r.readFull(c.Data[i*BlockSize : (i+1)*BlockSize]); err != nil {
			return Cluster{}, r.truncated("extent data", err)
		}
	}
	return c, nil
}

// readExtent reads and verifies one extent header and queues its entries.
func (r *Reader) readExtent() error {
	off := r.pos
	h := r.ext[:]
	err := r.readFull(h)
	if err == io.EOF {
		return r.complete()
	}
	if err != nil {
		return r.truncated("an extent header", err)
	}
	be := binary.BigEndian
	if !bytes.Equal(h[OffExtMagic:OffExtMagic+4], ExtentMagic[:]) {
		return formatErr(ErrMagic, off, "extent magic %q", h[OffExtMagic:OffExtMagic+4])
	}
	var stored [16]byte
	copy(stored[:], h[OffExtMD5:])
	clear(h[OffExtMD5 : OffExtMD5+16])
	if sum := md5.Sum(h); sum != stored {
		return formatErr(ErrExtentMD5, off, "stored %x, computed %x", stored, sum)
	}
	if v := be.Uint16(h[OffExtReserved:]); v != 0 {
		return formatErr(ErrReserved, off, "reserved %d", v)
	}
	if !bytes.Equal(h[OffExtUUID:OffExtUUID+16], r.h.UUID[:]) {
		return formatErr(ErrUUID, off, "uuid %x, header uuid %x", h[OffExtUUID:OffExtUUID+16], r.h.UUID)
	}
	entries := make([]pendingEntry, 0, MaxExtentClusters)
	blocks := 0
	for s := 0; s < MaxExtentClusters; s++ {
		info := be.Uint64(h[OffExtBlockInfo+8*s:])
		if info == 0 {
			continue
		}
		e := pendingEntry{mask: uint16(info >> 48), dev: uint8(info >> 32), index: uint32(info)}
		if rsv := uint8(info >> 40); rsv != 0 {
			return formatErr(ErrBlockInfo, off, "slot %d: reserved bits %#x", s, rsv)
		}
		d := r.devs[e.dev]
		if d == nil {
			return formatErr(ErrBlockInfo, off, "slot %d: device %d not in the header", s, e.dev)
		}
		if uint64(e.index) >= d.clusters {
			return formatErr(ErrBlockInfo, off, "slot %d: device %d cluster %d out of range (%d clusters)", s, e.dev, e.index, d.clusters)
		}
		if d.seen[e.index/64]&(1<<(e.index%64)) != 0 {
			return formatErr(ErrDuplicate, off, "slot %d: device %d cluster %d already seen", s, e.dev, e.index)
		}
		d.seen[e.index/64] |= 1 << (e.index % 64)
		d.count++
		for m := e.mask; m != 0; m &= m - 1 {
			blocks++
		}
		entries = append(entries, e)
	}
	if bc := int(be.Uint16(h[OffExtBlockCount:])); bc != blocks {
		return formatErr(ErrBlockInfo, off, "block_count %d, masks give %d", bc, blocks)
	}
	r.entries = entries
	return nil
}

// complete runs the end-of-stream check and returns io.EOF when it passes.
func (r *Reader) complete() error {
	for id, d := range r.devs {
		if d == nil || d.count == d.clusters {
			continue
		}
		var first uint64
		for first = 0; first < d.clusters; first++ {
			if d.seen[first/64]&(1<<(first%64)) == 0 {
				break
			}
		}
		return formatErr(ErrMissingCluster, r.pos, "device %d: %d of %d clusters missing (first: %d)", id, d.clusters-d.count, d.clusters, first)
	}
	return io.EOF
}
