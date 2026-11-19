// Package rvs implements RVS1. vmastream stores guest images in this format
// for deduplication.
//
// All integers are little-endian. The stream is a sequence of frames, each
// starting with a FrameHeaderSize-byte frame header:
//
//	0  magic "RVSF"   4 type   5 flags (0)   6 reserved u16 (0)
//	8  dev_id u32 (data/patch, else 0)       12 reserved u32 (0)
//	16 offset u64 (device byte offset of data/patch, multiple of ClusterSize, else 0)
//	24 length u64 (payload bytes that follow)
//	32 reserved [24] (0)   56 hcrc u32 = CRC-32C of bytes 0–55   60 reserved u32 (0)
//
// Frame sequence: header → config×k (VMA order) → for each device in
// ascending id: data frames at offsets 0, FrameBytes, 2·FrameBytes, … (the
// last one short), with patch frames anywhere before the data frame they
// patch → trailer → EOF.
//
// Determinism (Requirement 5.10): the header frame holds only the device
// table, data frames hold only device content at fixed offsets (patched
// positions as zero placeholders), and every per-run value (uuid, time,
// counters, warnings) lives in the trailer at the very end. The same device
// contents and table produce byte-identical frames up to the trailer.
// restic's content-defined chunker deduplicates these frames.
package rvs

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"

	"github.com/norandom/restic-pve/internal/stream/vma"
)

const (
	// ClusterSize is the unit of patches and of the sha256ct fingerprint.
	ClusterSize = vma.ClusterSize
	// FrameBytes is the payload size of a full data frame (64 MiB).
	FrameBytes = 64 << 20
	// FrameHeaderSize is the size of every frame header.
	FrameHeaderSize = 64
	// Version is the header payload version.
	Version = 1
	// Format is the trailer's "format" value.
	Format = "RVS1"

	// MaxTrailerBytes bounds the trailer payload.
	MaxTrailerBytes = 1 << 20
	// maxConfigPayload bounds a config frame: u16 name length, name, u32 data
	// length, data, each at most a VMA blob.
	maxConfigPayload = 2 + vma.MaxBlobSize + 4 + vma.MaxBlobSize
	// maxHeaderPayload bounds the header frame: fixed part plus 254 devices
	// with 255-byte names.
	maxHeaderPayload = headerFixed + (vma.MaxDevID-vma.MinDevID+1)*(1+1+255+8)
	headerFixed      = 4 + 4 + 4 + 4 + 4

	// frame header field offsets
	offMagic  = 0
	offType   = 4
	offFlags  = 5
	offDev    = 8
	offOffset = 16
	offLength = 24
	offCRC    = 56
)

var (
	// FrameMagic starts every frame header.
	FrameMagic = [4]byte{'R', 'V', 'S', 'F'}
	// PayloadMagic starts the header frame payload.
	PayloadMagic = [4]byte{'R', 'V', 'S', '1'}

	castagnoli = crc32.MakeTable(crc32.Castagnoli)
	le         = binary.LittleEndian
)

func crc32c(b []byte) uint32 { return crc32.Checksum(b, castagnoli) }

// FrameType is the type byte of a frame header.
type FrameType uint8

const (
	TypeHeader  FrameType = 1
	TypeConfig  FrameType = 2
	TypeData    FrameType = 3
	TypePatch   FrameType = 4
	TypeTrailer FrameType = 5
)

func (t FrameType) String() string {
	switch t {
	case TypeHeader:
		return "header"
	case TypeConfig:
		return "config"
	case TypeData:
		return "data"
	case TypePatch:
		return "patch"
	case TypeTrailer:
		return "trailer"
	}
	return fmt.Sprintf("type %d", uint8(t))
}

// FrameHeader is the decoded part of a frame header (all other fields are zero).
type FrameHeader struct {
	Type   FrameType
	Dev    uint32
	Offset uint64
	Length uint64
}

// Encode returns the 64-byte frame header including its CRC-32C.
func (h FrameHeader) Encode() [FrameHeaderSize]byte {
	var b [FrameHeaderSize]byte
	copy(b[offMagic:], FrameMagic[:])
	b[offType] = byte(h.Type)
	le.PutUint32(b[offDev:], h.Dev)
	le.PutUint64(b[offOffset:], h.Offset)
	le.PutUint64(b[offLength:], h.Length)
	le.PutUint32(b[offCRC:], crc32c(b[:offCRC]))
	return b
}

// DecodeFrameHeader checks magic, hcrc and the reserved fields of a 64-byte
// frame header. It does not check the type or the frame sequence. Errors are
// *FormatError with Offset 0 (the Reader sets the stream offset).
func DecodeFrameHeader(b []byte) (FrameHeader, error) {
	if len(b) != FrameHeaderSize {
		return FrameHeader{}, formatErr(ErrTruncated, 0, "frame header is %d bytes", len(b))
	}
	if !bytes.Equal(b[offMagic:offMagic+4], FrameMagic[:]) {
		return FrameHeader{}, formatErr(ErrMagic, 0, "frame magic %q", b[offMagic:offMagic+4])
	}
	if got, want := le.Uint32(b[offCRC:]), crc32c(b[:offCRC]); got != want {
		return FrameHeader{}, formatErr(ErrCRC, 0, "hcrc %08x, computed %08x", got, want)
	}
	for _, r := range [][2]int{{offFlags, offDev}, {12, offOffset}, {32, offCRC}, {60, FrameHeaderSize}} {
		if !allZero(b[r[0]:r[1]]) {
			return FrameHeader{}, formatErr(ErrReserved, 0, "reserved bytes %d–%d not zero", r[0], r[1]-1)
		}
	}
	return FrameHeader{
		Type:   FrameType(b[offType]),
		Dev:    le.Uint32(b[offDev:]),
		Offset: le.Uint64(b[offOffset:]),
		Length: le.Uint64(b[offLength:]),
	}, nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// --- errors ---

// ErrIntegrity is the class of every malformed-stream error (CLI exit 3).
// errors.Is(err, ErrIntegrity) holds for every *FormatError.
var ErrIntegrity = errors.New("rvs: integrity error")

// ErrSequence reports a writer used against its contract (sink calls out of
// cursor order, a patch behind the cursor, a trailer that does not match the
// device table, Close before every cluster). It is a programming error, not
// a property of the data.
var ErrSequence = errors.New("rvs: writer contract violated")

// ErrorKind classifies a structural defect of an RVS1 stream.
type ErrorKind int

const (
	// ErrMagic: frame magic or header payload magic is wrong.
	ErrMagic ErrorKind = iota + 1
	// ErrCRC: frame header CRC-32C mismatch.
	ErrCRC
	// ErrReserved: a reserved frame header field is not zero.
	ErrReserved
	// ErrFrameType: unknown frame type.
	ErrFrameType
	// ErrOrder: a frame out of sequence (header not first, config after the
	// body, data frame for the wrong device or offset, patch after the data
	// frame it patches, trailer before the last data frame).
	ErrOrder
	// ErrOffset: unknown device, misaligned offset or a range beyond the device.
	ErrOffset
	// ErrLength: a frame length that does not match its position or bounds.
	ErrLength
	// ErrPayload: malformed header, config or trailer payload, or a trailer
	// inconsistent with the device table.
	ErrPayload
	// ErrTruncated: the stream ends before the end of the trailer.
	ErrTruncated
	// ErrTrailingData: bytes after the trailer.
	ErrTrailingData
)

var kindNames = map[ErrorKind]string{
	ErrMagic:        "bad magic",
	ErrCRC:          "frame header crc mismatch",
	ErrReserved:     "reserved field not zero",
	ErrFrameType:    "unknown frame type",
	ErrOrder:        "frame out of order",
	ErrOffset:       "bad frame offset",
	ErrLength:       "bad frame length",
	ErrPayload:      "malformed payload",
	ErrTruncated:    "truncated stream",
	ErrTrailingData: "data after the trailer",
}

func (k ErrorKind) String() string {
	if s, ok := kindNames[k]; ok {
		return s
	}
	return fmt.Sprintf("ErrorKind(%d)", int(k))
}

// FormatError reports a structural defect at a stream offset (the start of
// the offending frame, or the end of the data read for truncation).
type FormatError struct {
	Kind   ErrorKind
	Offset int64
	Detail string
}

func (e *FormatError) Error() string {
	return fmt.Sprintf("rvs: %s at offset %d: %s", e.Kind, e.Offset, e.Detail)
}

// Unwrap places every format error in the integrity class.
func (e *FormatError) Unwrap() error { return ErrIntegrity }

func formatErr(kind ErrorKind, off int64, format string, args ...any) *FormatError {
	return &FormatError{Kind: kind, Offset: off, Detail: fmt.Sprintf(format, args...)}
}

// --- header payload ---

// Header is the decoded header and config frames.
type Header struct {
	Devices []vma.DevInfo // ascending ID
	Configs []vma.ConfigBlob
}

func validateDevices(devs []vma.DevInfo) error {
	for i, d := range devs {
		if d.ID < vma.MinDevID || d.ID > vma.MaxDevID {
			return fmt.Errorf("device id %d out of range", d.ID)
		}
		if i > 0 && d.ID <= devs[i-1].ID {
			return fmt.Errorf("device ids not strictly ascending (%d after %d)", d.ID, devs[i-1].ID)
		}
		if len(d.Name) == 0 || len(d.Name) > 255 {
			return fmt.Errorf("device %d name length %d", d.ID, len(d.Name))
		}
	}
	return nil
}

// encodeHeaderPayload: "RVS1", u32 version, u32 cluster_size, u32
// frame_bytes, u32 device_count, then per device u8 id, u8 name_len, name,
// u64 size. No uuid, time or run data.
func encodeHeaderPayload(devs []vma.DevInfo) []byte {
	b := append([]byte(nil), PayloadMagic[:]...)
	b = le.AppendUint32(b, Version)
	b = le.AppendUint32(b, ClusterSize)
	b = le.AppendUint32(b, FrameBytes)
	b = le.AppendUint32(b, uint32(len(devs)))
	for _, d := range devs {
		b = append(b, d.ID, byte(len(d.Name)))
		b = append(b, d.Name...)
		b = le.AppendUint64(b, d.Size)
	}
	return b
}

func decodeHeaderPayload(p []byte) ([]vma.DevInfo, error) {
	if len(p) < headerFixed {
		return nil, fmt.Errorf("header payload %d bytes", len(p))
	}
	if !bytes.Equal(p[:4], PayloadMagic[:]) {
		return nil, fmt.Errorf("payload magic %q", p[:4])
	}
	if v := le.Uint32(p[4:]); v != Version {
		return nil, fmt.Errorf("version %d", v)
	}
	if v := le.Uint32(p[8:]); v != ClusterSize {
		return nil, fmt.Errorf("cluster size %d", v)
	}
	if v := le.Uint32(p[12:]); v != FrameBytes {
		return nil, fmt.Errorf("frame bytes %d", v)
	}
	n := le.Uint32(p[16:])
	if n > vma.MaxDevID {
		return nil, fmt.Errorf("device count %d", n)
	}
	p = p[headerFixed:]
	devs := make([]vma.DevInfo, 0, n)
	for i := uint32(0); i < n; i++ {
		if len(p) < 2 || len(p) < 2+int(p[1])+8 {
			return nil, fmt.Errorf("device entry %d truncated", i)
		}
		l := int(p[1])
		devs = append(devs, vma.DevInfo{ID: p[0], Name: string(p[2 : 2+l]), Size: le.Uint64(p[2+l:])})
		p = p[2+l+8:]
	}
	if len(p) != 0 {
		return nil, fmt.Errorf("%d bytes after the device table", len(p))
	}
	if err := validateDevices(devs); err != nil {
		return nil, err
	}
	return devs, nil
}

// config payload: u16 name_len, name, u32 data_len, data (verbatim).
func encodeConfigPayload(c vma.ConfigBlob) []byte {
	b := le.AppendUint16(nil, uint16(len(c.Name)))
	b = append(b, c.Name...)
	b = le.AppendUint32(b, uint32(len(c.Data)))
	return append(b, c.Data...)
}

func decodeConfigPayload(p []byte) (vma.ConfigBlob, error) {
	if len(p) < 2 {
		return vma.ConfigBlob{}, errors.New("config payload truncated")
	}
	nl := int(le.Uint16(p))
	if len(p) < 2+nl+4 {
		return vma.ConfigBlob{}, errors.New("config name truncated")
	}
	name := string(p[2 : 2+nl])
	dl := int(le.Uint32(p[2+nl:]))
	if len(p) != 2+nl+4+dl {
		return vma.ConfigBlob{}, fmt.Errorf("config %q data length %d does not match the frame", name, dl)
	}
	if nl == 0 {
		return vma.ConfigBlob{}, errors.New("empty config name")
	}
	return vma.ConfigBlob{Name: name, Data: append([]byte(nil), p[2+nl+4:]...)}, nil
}

// --- trailer ---

// TrailerDevice is one device entry of the trailer (and of the manifest).
type TrailerDevice struct {
	ID             uint8  `json:"id"`
	Name           string `json:"name"`
	Size           uint64 `json:"size"`
	SHA256CT       string `json:"sha256ct"` // lowercase hex, see package verify
	AllocatedBytes uint64 `json:"allocated_bytes"`
	ZeroClusters   uint64 `json:"zero_clusters"`
}

// Trailer is the final frame's payload, encoded as canonical JSON. It holds
// every per-run value of the stream.
type Trailer struct {
	Format         string          `json:"format"`
	VMAUUID        string          `json:"vma_uuid"`
	VMACTime       int64           `json:"vma_ctime"`
	Devices        []TrailerDevice `json:"devices"`
	PatchCount     uint64          `json:"patch_count"`
	EarlyClusters  uint64          `json:"early_clusters"`
	BufferRAMPeak  int64           `json:"buffer_ram_peak"`
	Consistency    string          `json:"consistency"`
	VzdumpWarnings []string        `json:"vzdump_warnings"`
	Tool           string          `json:"tool"`
}

// MarshalTrailer encodes t as canonical JSON with format "RVS1" (empty lists
// as [], never null).
func MarshalTrailer(t Trailer) ([]byte, error) {
	t.Format = Format
	if t.Devices == nil {
		t.Devices = []TrailerDevice{}
	}
	if t.VzdumpWarnings == nil {
		t.VzdumpWarnings = []string{}
	}
	return CanonicalJSON(t)
}

// CanonicalJSON encodes v as compact JSON with object keys sorted at every
// level, UTF-8 without HTML escaping, numbers preserved exactly.
func CanonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(generic); err != nil { // maps encode with sorted keys
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// checkTrailerDevices verifies that the trailer's device list matches the
// device table and that every digest is 64 lowercase hex characters.
func checkTrailerDevices(t *Trailer, devs []vma.DevInfo) error {
	if len(t.Devices) != len(devs) {
		return fmt.Errorf("trailer lists %d devices, header %d", len(t.Devices), len(devs))
	}
	for i, d := range devs {
		td := t.Devices[i]
		if td.ID != d.ID || td.Name != d.Name || td.Size != d.Size {
			return fmt.Errorf("trailer device %d (%d %q %d) differs from header (%d %q %d)",
				i, td.ID, td.Name, td.Size, d.ID, d.Name, d.Size)
		}
		if !isHexDigest(td.SHA256CT) {
			return fmt.Errorf("device %d sha256ct %q is not 64 lowercase hex characters", d.ID, td.SHA256CT)
		}
	}
	return nil
}

func isHexDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// dataFrameLength is the length of the data frame at offset of a device.
func dataFrameLength(size, offset uint64) uint64 { return min(FrameBytes, size-offset) }
