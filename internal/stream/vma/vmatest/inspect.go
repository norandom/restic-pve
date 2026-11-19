package vmatest

import (
	"crypto/md5"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/norandom/restic-pve/internal/stream/vma"
)

// Layout decodes the structure of a VMA stream. It reports the bytes
// (including computed md5s) without rejecting bad input. Tests can check a
// writer's output and see exactly what a tampering hook changed.
type Layout struct {
	Magic             [4]byte
	Version           uint32
	UUID              [16]byte
	CTime             int64
	HeaderMD5         [16]byte // as stored
	ComputedHeaderMD5 [16]byte // over header_size bytes with the md5 field zeroed
	BlobBufferOffset  uint32
	BlobBufferSize    uint32
	HeaderSize        uint32
	Devices           []vma.DevInfo // dev_info entries with a non-zero size, ascending id
	Configs           []vma.ConfigBlob
	Extents           []Extent
	// TrailingBytes counts bytes after the last complete extent (a truncated extent).
	TrailingBytes int
}

// HeaderMD5OK reports whether the stored header md5 matches.
func (l *Layout) HeaderMD5OK() bool { return l.HeaderMD5 == l.ComputedHeaderMD5 }

// Extent is one decoded extent.
type Extent struct {
	Offset      int64
	Magic       [4]byte
	Reserved    uint16
	BlockCount  uint16
	UUID        [16]byte
	MD5         [16]byte // as stored
	ComputedMD5 [16]byte // over the 512-byte header with the md5 field zeroed
	Entries     []Entry  // non-empty blockinfo slots, in slot order
	// Slots is the number of blockinfo slots used (non-zero entries).
	Slots int
}

// MD5OK reports whether the stored extent md5 matches.
func (e *Extent) MD5OK() bool { return e.MD5 == e.ComputedMD5 }

// Entry is one blockinfo slot and its data blocks.
type Entry struct {
	Slot     int
	Mask     uint16
	Reserved uint8
	Dev      uint8
	Index    uint32
	Data     []byte // packed blocks, BlockSize per set mask bit
}

// Cluster expands the entry to a full cluster (zeros where mask bits are clear).
func (e Entry) Cluster() []byte {
	out := make([]byte, vma.ClusterSize)
	pos := 0
	for i := 0; i < vma.BlocksPerCluster; i++ {
		if e.Mask&(1<<i) != 0 && pos+vma.BlockSize <= len(e.Data) {
			copy(out[i*vma.BlockSize:], e.Data[pos:pos+vma.BlockSize])
			pos += vma.BlockSize
		}
	}
	return out
}

// Inspect decodes b. It fails only when the header itself cannot be decoded.
func Inspect(b []byte) (*Layout, error) {
	if len(b) < vma.HeaderFixedSize {
		return nil, fmt.Errorf("stream shorter than the fixed header (%d bytes)", len(b))
	}
	be := binary.BigEndian
	l := &Layout{}
	copy(l.Magic[:], b[vma.OffMagic:])
	l.Version = be.Uint32(b[vma.OffVersion:])
	copy(l.UUID[:], b[vma.OffUUID:])
	l.CTime = int64(be.Uint64(b[vma.OffCTime:]))
	copy(l.HeaderMD5[:], b[vma.OffMD5:])
	l.BlobBufferOffset = be.Uint32(b[vma.OffBlobBufferOffset:])
	l.BlobBufferSize = be.Uint32(b[vma.OffBlobBufferSize:])
	l.HeaderSize = be.Uint32(b[vma.OffHeaderSize:])
	if int(l.HeaderSize) < vma.HeaderFixedSize || int(l.HeaderSize) > len(b) {
		return nil, fmt.Errorf("header_size %d out of range", l.HeaderSize)
	}
	if uint64(l.BlobBufferOffset)+uint64(l.BlobBufferSize) > uint64(l.HeaderSize) {
		return nil, errors.New("blob buffer exceeds header")
	}
	head := append([]byte(nil), b[:l.HeaderSize]...)
	clear(head[vma.OffMD5 : vma.OffMD5+16])
	l.ComputedHeaderMD5 = md5.Sum(head)

	blobs := b[l.BlobBufferOffset : l.BlobBufferOffset+l.BlobBufferSize]
	blob := func(ptr uint32) ([]byte, error) {
		if ptr == 0 || uint64(ptr)+2 > uint64(len(blobs)) {
			return nil, fmt.Errorf("blob pointer %d out of range", ptr)
		}
		n := uint32(binary.LittleEndian.Uint16(blobs[ptr:]))
		if uint64(ptr)+2+uint64(n) > uint64(len(blobs)) {
			return nil, fmt.Errorf("blob at %d exceeds buffer", ptr)
		}
		return blobs[ptr+2 : ptr+2+n], nil
	}
	str := func(ptr uint32) (string, error) {
		d, err := blob(ptr)
		if err != nil {
			return "", err
		}
		if len(d) == 0 || d[len(d)-1] != 0 {
			return "", fmt.Errorf("string blob at %d not NUL-terminated", ptr)
		}
		return string(d[:len(d)-1]), nil
	}
	for i := 0; i < vma.MaxConfigs; i++ {
		np := be.Uint32(b[vma.OffConfigNames+4*i:])
		dp := be.Uint32(b[vma.OffConfigData+4*i:])
		if np == 0 && dp == 0 {
			continue
		}
		name, err := str(np)
		if err != nil {
			return nil, fmt.Errorf("config %d name: %w", i, err)
		}
		data, err := blob(dp)
		if err != nil {
			return nil, fmt.Errorf("config %d data: %w", i, err)
		}
		l.Configs = append(l.Configs, vma.ConfigBlob{Name: name, Data: append([]byte(nil), data...)})
	}
	for id := 0; id < 256; id++ {
		off := vma.OffDevInfo + id*vma.DevInfoSize
		size := be.Uint64(b[off+8:])
		if size == 0 {
			continue
		}
		name, err := str(be.Uint32(b[off:]))
		if err != nil {
			return nil, fmt.Errorf("device %d name: %w", id, err)
		}
		l.Devices = append(l.Devices, vma.DevInfo{ID: uint8(id), Name: name, Size: size})
	}

	pos := int(l.HeaderSize)
	for pos < len(b) {
		if len(b)-pos < vma.ExtentHeaderSize {
			l.TrailingBytes = len(b) - pos
			break
		}
		h := b[pos : pos+vma.ExtentHeaderSize]
		e := Extent{Offset: int64(pos)}
		copy(e.Magic[:], h[vma.OffExtMagic:])
		e.Reserved = be.Uint16(h[vma.OffExtReserved:])
		e.BlockCount = be.Uint16(h[vma.OffExtBlockCount:])
		copy(e.UUID[:], h[vma.OffExtUUID:])
		copy(e.MD5[:], h[vma.OffExtMD5:])
		z := append([]byte(nil), h...)
		clear(z[vma.OffExtMD5 : vma.OffExtMD5+16])
		e.ComputedMD5 = md5.Sum(z)
		dataLen := int(e.BlockCount) * vma.BlockSize
		if len(b)-pos-vma.ExtentHeaderSize < dataLen {
			l.TrailingBytes = len(b) - pos
			break
		}
		data := b[pos+vma.ExtentHeaderSize : pos+vma.ExtentHeaderSize+dataLen]
		dpos := 0
		for s := 0; s < vma.MaxExtentClusters; s++ {
			info := be.Uint64(h[vma.OffExtBlockInfo+8*s:])
			if info == 0 {
				continue
			}
			en := Entry{
				Slot:     s,
				Mask:     uint16(info >> 48),
				Reserved: uint8(info >> 40),
				Dev:      uint8(info >> 32),
				Index:    uint32(info),
			}
			n := popcount(en.Mask) * vma.BlockSize
			if dpos+n <= len(data) {
				en.Data = data[dpos : dpos+n]
			}
			dpos += n
			e.Entries = append(e.Entries, en)
			e.Slots++
		}
		l.Extents = append(l.Extents, e)
		pos += vma.ExtentHeaderSize + dataLen
	}
	return l, nil
}

// Contents reassembles every device from the decoded extents (later entries
// overwrite earlier ones). Each device is truncated to its declared size.
func (l *Layout) Contents() map[uint8][]byte {
	out := map[uint8][]byte{}
	for _, d := range l.Devices {
		out[d.ID] = make([]byte, vma.Clusters(d.Size)*vma.ClusterSize)
	}
	for _, e := range l.Extents {
		for _, en := range e.Entries {
			buf, ok := out[en.Dev]
			if !ok || uint64(en.Index) >= uint64(len(buf))/vma.ClusterSize {
				continue
			}
			copy(buf[uint64(en.Index)*vma.ClusterSize:], en.Cluster())
		}
	}
	for _, d := range l.Devices {
		out[d.ID] = out[d.ID][:d.Size]
	}
	return out
}

func popcount(m uint16) int {
	n := 0
	for ; m != 0; m &= m - 1 {
		n++
	}
	return n
}
