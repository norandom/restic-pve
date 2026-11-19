// Package vmatest builds synthetic VMA streams for tests: arbitrary devices,
// configs, a chosen cluster arrival order and tampering hooks. Streams are
// produced by vma.Writer, then optionally modified by exactly one tamper.
//
// Everything is held in memory: the whole stream and every device's contents.
// Keep test devices in the MiB range.
package vmatest

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"math/rand"
	"sort"

	"github.com/norandom/restic-pve/internal/stream/vma"
)

// Device is one synthetic device. Data holds its content (len ≤ Size, the rest is zeros).
type Device struct {
	ID   uint8
	Name string
	Size uint64
	Data []byte
}

// ClusterRef names one cluster of one device.
type ClusterRef struct {
	Dev   uint8
	Index uint32
}

// TamperKind selects a defect injected after the stream is built.
type TamperKind int

const (
	TamperNone TamperKind = iota
	// TamperHeaderMD5 flips a byte of the header md5. Offset: 0.
	TamperHeaderMD5
	// TamperExtentMD5 flips a byte of the extent md5. Offset: the extent.
	TamperExtentMD5
	// TamperExtentUUID changes the extent uuid (md5 recomputed). Offset: the extent.
	TamperExtentUUID
	// TamperReserved sets the extent's reserved field to 1 (md5 recomputed). Offset: the extent.
	TamperReserved
	// TamperBlockInfoDevice points the extent's first blockinfo at a device id
	// absent from the header (md5 recomputed). Offset: the extent.
	TamperBlockInfoDevice
	// TamperBlockInfoRange sets the first blockinfo's cluster index to the
	// device's cluster count (md5 recomputed). Offset: the extent.
	TamperBlockInfoRange
	// TamperBlockInfoReserved sets blockinfo bits 47–40 of the first entry
	// (md5 recomputed). Offset: the extent.
	TamperBlockInfoReserved
	// TamperDuplicateCluster inserts a copy of the extent right after it, so
	// its clusters arrive twice. Offset: the copy.
	TamperDuplicateCluster
	// TamperMissingCluster removes the extent, so its clusters never arrive.
	// Offset: the end of the stream (where a reader notices).
	TamperMissingCluster
	// TamperTruncate cuts the stream at At, or (At ≤ 0) inside the final
	// extent. Offset: the end of the cut stream.
	TamperTruncate
)

// Tamper describes one defect. Extent is the index of the affected extent.
type Tamper struct {
	Kind   TamperKind
	Extent int
	At     int64
}

// Builder describes a synthetic stream. Zero UUID/CTime mean the writer's
// defaults (fresh uuid, current time); fix them for reproducible streams.
// A nil Order means SequentialOrder. Device Data and the built stream are held
// in memory, so keep test devices in the MiB range.
type Builder struct {
	UUID    [16]byte
	CTime   int64
	Devices []Device
	Configs []vma.ConfigBlob
	Order   []ClusterRef
	Tamper  Tamper
}

// Result is a built stream.
type Result struct {
	Stream        []byte
	UUID          [16]byte
	HeaderSize    int64
	ExtentOffsets []int64 // complete extents of the final (possibly tampered) stream
	TamperOffset  int64   // see TamperKind
}

// Contents returns the expected content of a device (Size bytes), or nil.
func (b *Builder) Contents(dev uint8) []byte {
	for _, d := range b.Devices {
		if d.ID == dev {
			out := make([]byte, d.Size)
			copy(out, d.Data)
			return out
		}
	}
	return nil
}

// Build writes the stream and applies the tamper. The whole stream (and each
// device's contents) is built in memory; keep test devices in the MiB range.
func (b *Builder) Build() (*Result, error) {
	devs := make([]vma.DevInfo, 0, len(b.Devices))
	contents := map[uint8][]byte{}
	for _, d := range b.Devices {
		if uint64(len(d.Data)) > d.Size {
			return nil, fmt.Errorf("vmatest: device %d: %d data bytes exceed size %d", d.ID, len(d.Data), d.Size)
		}
		devs = append(devs, vma.DevInfo{ID: d.ID, Name: d.Name, Size: d.Size})
		contents[d.ID] = b.Contents(d.ID)
	}
	sort.Slice(devs, func(i, j int) bool { return devs[i].ID < devs[j].ID })
	var buf bytes.Buffer
	w, err := vma.NewWriter(&buf, vma.WriterOptions{UUID: b.UUID, CTime: b.CTime, Devices: devs, Configs: b.Configs})
	if err != nil {
		return nil, err
	}
	order := b.Order
	if order == nil {
		order = SequentialOrder(b.Devices)
	}
	for _, r := range order {
		c := contents[r.Dev]
		var data []byte
		if lo := uint64(r.Index) * vma.ClusterSize; c != nil && lo < uint64(len(c)) {
			data = c[lo:min(lo+vma.ClusterSize, uint64(len(c)))]
		}
		if err := w.WriteCluster(r.Dev, r.Index, data); err != nil {
			w.Abort()
			return nil, err
		}
	}
	if err := w.Commit(); err != nil {
		w.Abort()
		return nil, err
	}
	stream := buf.Bytes()
	res := &Result{UUID: w.UUID()}
	stream, res.TamperOffset, err = applyTamper(stream, b.Tamper, b.Devices)
	if err != nil {
		return nil, err
	}
	res.Stream = stream
	l, err := Inspect(stream)
	if err != nil {
		if b.Tamper.Kind == TamperTruncate { // cut inside the header: no layout to report
			return res, nil
		}
		return nil, err
	}
	res.HeaderSize = int64(l.HeaderSize)
	for _, e := range l.Extents {
		res.ExtentOffsets = append(res.ExtentOffsets, e.Offset)
	}
	return res, nil
}

func applyTamper(s []byte, t Tamper, devs []Device) ([]byte, int64, error) {
	if t.Kind == TamperNone {
		return s, 0, nil
	}
	if t.Kind == TamperHeaderMD5 {
		s[vma.OffMD5] ^= 0xFF
		return s, 0, nil
	}
	l, err := Inspect(s)
	if err != nil {
		return nil, 0, err
	}
	if t.Kind == TamperTruncate {
		at := t.At
		if at <= 0 {
			last := l.Extents[len(l.Extents)-1]
			at = last.Offset + vma.ExtentHeaderSize/2
			if last.BlockCount > 0 {
				at = last.Offset + vma.ExtentHeaderSize + vma.BlockSize/2
			}
		}
		if at >= int64(len(s)) {
			return nil, 0, fmt.Errorf("vmatest: truncation at %d beyond stream length %d", at, len(s))
		}
		return s[:at], at, nil
	}
	if t.Extent < 0 || t.Extent >= len(l.Extents) {
		return nil, 0, fmt.Errorf("vmatest: tamper extent %d, stream has %d", t.Extent, len(l.Extents))
	}
	e := l.Extents[t.Extent]
	off := e.Offset
	end := off + vma.ExtentHeaderSize + int64(e.BlockCount)*vma.BlockSize
	h := s[off : off+vma.ExtentHeaderSize]
	be := binary.BigEndian
	firstInfo := func() (int, uint64) {
		slot := e.Entries[0].Slot
		return vma.OffExtBlockInfo + 8*slot, be.Uint64(h[vma.OffExtBlockInfo+8*slot:])
	}
	switch t.Kind {
	case TamperExtentMD5:
		h[vma.OffExtMD5] ^= 0xFF
		return s, off, nil
	case TamperExtentUUID:
		h[vma.OffExtUUID] ^= 0xFF
	case TamperReserved:
		be.PutUint16(h[vma.OffExtReserved:], 1)
	case TamperBlockInfoDevice:
		used := map[uint8]bool{}
		for _, d := range devs {
			used[d.ID] = true
		}
		id := uint8(vma.MaxDevID)
		for used[id] {
			id--
		}
		p, info := firstInfo()
		be.PutUint64(h[p:], info&^(0xFF<<32)|uint64(id)<<32)
	case TamperBlockInfoRange:
		p, info := firstInfo()
		dev := uint8(info >> 32)
		var n uint64
		for _, d := range devs {
			if d.ID == dev {
				n = vma.Clusters(d.Size)
			}
		}
		be.PutUint64(h[p:], info&^0xFFFFFFFF|n&0xFFFFFFFF)
	case TamperBlockInfoReserved:
		p, info := firstInfo()
		be.PutUint64(h[p:], info|0xFF<<40)
	case TamperDuplicateCluster:
		out := append([]byte(nil), s[:end]...)
		out = append(out, s[off:end]...)
		out = append(out, s[end:]...)
		return out, end, nil
	case TamperMissingCluster:
		out := append(append([]byte(nil), s[:off]...), s[end:]...)
		return out, int64(len(out)), nil
	default:
		return nil, 0, fmt.Errorf("vmatest: unknown tamper kind %d", t.Kind)
	}
	reseal(h)
	return s, off, nil
}

// reseal recomputes the md5 of a 512-byte extent header.
func reseal(h []byte) {
	clear(h[vma.OffExtMD5 : vma.OffExtMD5+16])
	sum := md5.Sum(h)
	copy(h[vma.OffExtMD5:], sum[:])
}

// SequentialOrder lists every cluster by ascending device id, then index.
func SequentialOrder(devs []Device) []ClusterRef {
	sorted := append([]Device(nil), devs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	var out []ClusterRef
	for _, d := range sorted {
		for i := uint64(0); i < vma.Clusters(d.Size); i++ {
			out = append(out, ClusterRef{d.ID, uint32(i)})
		}
	}
	return out
}

// ShuffledOrder is a deterministic random permutation of SequentialOrder.
func ShuffledOrder(devs []Device, seed int64) []ClusterRef {
	out := SequentialOrder(devs)
	rand.New(rand.NewSource(seed)).Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// InterleavedOrder spreads every device's clusters evenly over the whole
// stream (each device in ascending index), so the clusters of small devices
// arrive throughout the large one.
func InterleavedOrder(devs []Device) []ClusterRef {
	out := SequentialOrder(devs)
	count := map[uint8]uint64{}
	for _, d := range devs {
		count[d.ID] = vma.Clusters(d.Size)
	}
	// position of cluster i of an n-cluster device: (2i+1)/(2n)
	sort.SliceStable(out, func(a, b int) bool {
		ka := (2*uint64(out[a].Index) + 1) * count[out[b].Dev]
		kb := (2*uint64(out[b].Index) + 1) * count[out[a].Dev]
		if ka != kb {
			return ka < kb
		}
		return out[a].Dev < out[b].Dev
	})
	return out
}

// PatternData returns deterministic content of the given size: random data in
// which every fifth cluster (from cluster 1) is all zeros and every fifth
// cluster (from cluster 3) has its even 4 KiB blocks zeroed.
func PatternData(size int, seed int64) []byte {
	b := make([]byte, size)
	rand.New(rand.NewSource(seed)).Read(b)
	for c := 0; c*vma.ClusterSize < size; c++ {
		lo := c * vma.ClusterSize
		hi := min(lo+vma.ClusterSize, size)
		switch c % 5 {
		case 1:
			clear(b[lo:hi])
		case 3:
			for blk := lo; blk < hi; blk += 2 * vma.BlockSize {
				clear(b[blk:min(blk+vma.BlockSize, hi)])
			}
		}
	}
	return b
}

// DataDisk is a data disk "drive-<key>" (key e.g. "scsi0") filled with PatternData.
func DataDisk(id uint8, key string, size uint64, seed int64) Device {
	return Device{ID: id, Name: "drive-" + key, Size: size, Data: PatternData(int(size), seed)}
}

// EFIDisk is an EFI vars disk "drive-efidisk0" filled with PatternData.
func EFIDisk(id uint8, size uint64, seed int64) Device {
	return Device{ID: id, Name: "drive-efidisk0", Size: size, Data: PatternData(int(size), seed)}
}

// TPMState is a TPM state device "drive-tpmstate0-backup" filled with PatternData.
func TPMState(id uint8, size uint64, seed int64) Device {
	return Device{ID: id, Name: "drive-tpmstate0-backup", Size: size, Data: PatternData(int(size), seed)}
}

// VMStateDevice is a memory state device "vmstate" (must be rejected by a reader).
func VMStateDevice(id uint8, size uint64) Device {
	return Device{ID: id, Name: "vmstate", Size: size, Data: PatternData(int(size), int64(id))}
}

// ExampleConfig is a neutral qemu-server.conf with "#qmdump#map:" lines for
// drive-scsi0, drive-efidisk0 and drive-tpmstate0-backup.
func ExampleConfig() vma.ConfigBlob {
	return vma.ConfigBlob{Name: "qemu-server.conf", Data: []byte(`#qmdump#map:efidisk0:drive-efidisk0:fast-ssd:raw:
#qmdump#map:scsi0:drive-scsi0:fast-ssd:raw:
#qmdump#map:tpmstate0:drive-tpmstate0-backup:fast-ssd:raw:
agent: 1
bios: ovmf
boot: order=scsi0
cores: 2
efidisk0: fast-ssd:vm-100-disk-1,efitype=4m,pre-enrolled-keys=1,size=528K
machine: q35
memory: 4096
name: example
net0: virtio=02:00:00:00:01:00,bridge=vmbr0
ostype: l26
scsi0: fast-ssd:vm-100-disk-0,discard=on,iothread=1,size=32G
scsihw: virtio-scsi-single
tpmstate0: fast-ssd:vm-100-disk-2,size=4M,version=v2.0
`)}
}
