package vma

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// WriterOptions configures a Writer. A zero UUID is replaced by a fresh random
// one and a zero CTime by the current time.
type WriterOptions struct {
	UUID    [16]byte
	CTime   int64
	Devices []DevInfo
	Configs []ConfigBlob
}

type writerState int

const (
	stateOpen writerState = iota
	stateCommitted
	stateAborted
)

type devState struct {
	size     uint64
	clusters uint64
	seen     []uint64 // bitmap of written clusters
	written  uint64
}

// Writer writes a VMA v1 stream. NewWriter writes the header, followed by
// extents of at most WriterExtentClusters clusters (as the pve vma-writer).
// Clusters may arrive in any order. Each must be written exactly once.
// Commit releases the final extent. If the producer fails (Abort, or no
// Commit), the stream stays truncated and every VMA reader rejects it.
type Writer struct {
	w     io.Writer
	uuid  [16]byte
	devs  [256]*devState
	state writerState
	err   error // sticky write error

	infos []uint64 // pending extent blockinfo entries
	data  []byte   // pending extent data blocks
}

// NewWriter validates the options and writes the header.
func NewWriter(w io.Writer, o WriterOptions) (*Writer, error) {
	vw := &Writer{w: w, uuid: o.UUID}
	if vw.uuid == ([16]byte{}) {
		if _, err := rand.Read(vw.uuid[:]); err != nil {
			return nil, fmt.Errorf("vma: generate uuid: %w", err)
		}
	}
	ctime := o.CTime
	if ctime == 0 {
		ctime = time.Now().Unix()
	}
	head, err := encodeHeader(vw.uuid, ctime, o.Devices, o.Configs)
	if err != nil {
		return nil, err
	}
	for _, d := range o.Devices {
		n := Clusters(d.Size)
		vw.devs[d.ID] = &devState{size: d.Size, clusters: n, seen: make([]uint64, (n+63)/64)}
	}
	if _, err := w.Write(head); err != nil {
		return nil, fmt.Errorf("vma: write header: %w", err)
	}
	vw.infos = make([]uint64, 0, WriterExtentClusters)
	vw.data = make([]byte, 0, WriterExtentClusters*ClusterSize)
	return vw, nil
}

func validateOptions(devs []DevInfo, cfgs []ConfigBlob) error {
	if len(devs) == 0 {
		return errors.New("vma: no devices")
	}
	ids := map[uint8]bool{}
	names := map[string]bool{}
	for _, d := range devs {
		switch {
		case d.ID < MinDevID || d.ID > MaxDevID:
			return fmt.Errorf("vma: device %q: id %d outside %d..%d", d.Name, d.ID, MinDevID, MaxDevID)
		case ids[d.ID]:
			return fmt.Errorf("vma: duplicate device id %d", d.ID)
		case d.Name == "" || strings.IndexByte(d.Name, 0) >= 0 || len(d.Name)+1 > MaxBlobSize:
			return fmt.Errorf("vma: device %d: invalid name %q", d.ID, d.Name)
		case names[d.Name]:
			return fmt.Errorf("vma: duplicate device name %q", d.Name)
		case d.Size == 0:
			return fmt.Errorf("vma: device %q: size 0", d.Name)
		case Clusters(d.Size) > 1<<32:
			return fmt.Errorf("vma: device %q: size %d too large", d.Name, d.Size)
		}
		ids[d.ID] = true
		names[d.Name] = true
	}
	if len(cfgs) > MaxConfigs {
		return fmt.Errorf("vma: %d configs, at most %d", len(cfgs), MaxConfigs)
	}
	for i, c := range cfgs {
		if c.Name == "" || strings.IndexByte(c.Name, 0) >= 0 || len(c.Name)+1 > MaxBlobSize {
			return fmt.Errorf("vma: config %d: invalid name %q", i, c.Name)
		}
		if len(c.Data) > MaxBlobSize {
			return fmt.Errorf("vma: config %q: %d bytes, at most %d", c.Name, len(c.Data), MaxBlobSize)
		}
	}
	return nil
}

// encodeHeader returns the complete header (fixed part plus padded blob buffer)
// with its md5 set.
func encodeHeader(uuid [16]byte, ctime int64, devs []DevInfo, cfgs []ConfigBlob) ([]byte, error) {
	if err := validateOptions(devs, cfgs); err != nil {
		return nil, err
	}
	blobs := []byte{0} // position 0 is never used
	addBlob := func(data []byte) uint32 {
		pos := uint32(len(blobs))
		blobs = binary.LittleEndian.AppendUint16(blobs, uint16(len(data)))
		blobs = append(blobs, data...)
		return pos
	}
	addString := func(s string) uint32 { return addBlob(append([]byte(s), 0)) }

	head := make([]byte, HeaderFixedSize)
	be := binary.BigEndian
	for i, c := range cfgs {
		be.PutUint32(head[OffConfigNames+4*i:], addString(c.Name))
		be.PutUint32(head[OffConfigData+4*i:], addBlob(c.Data))
	}
	for _, d := range devs {
		off := OffDevInfo + int(d.ID)*DevInfoSize
		be.PutUint32(head[off:], addString(d.Name))
		be.PutUint64(head[off+8:], d.Size)
	}
	padded := (len(blobs) + 511) / 512 * 512
	if uint64(HeaderFixedSize)+uint64(padded) > 1<<32-1 {
		return nil, errors.New("vma: header too large")
	}
	copy(head[OffMagic:], HeaderMagic[:])
	be.PutUint32(head[OffVersion:], Version)
	copy(head[OffUUID:], uuid[:])
	be.PutUint64(head[OffCTime:], uint64(ctime))
	be.PutUint32(head[OffBlobBufferOffset:], HeaderFixedSize)
	be.PutUint32(head[OffBlobBufferSize:], uint32(len(blobs)))
	be.PutUint32(head[OffHeaderSize:], uint32(HeaderFixedSize+padded))
	head = append(head, blobs...)
	head = append(head, make([]byte, padded-len(blobs))...)
	sum := md5.Sum(head) // md5 field is still zero
	copy(head[OffMD5:], sum[:])
	return head, nil
}

// UUID returns the stream uuid (header and every extent).
func (w *Writer) UUID() [16]byte { return w.uuid }

func (w *Writer) usable() error {
	if w.err != nil {
		return w.err
	}
	switch w.state {
	case stateCommitted:
		return errors.New("vma: writer already committed")
	case stateAborted:
		return errors.New("vma: writer aborted")
	}
	return nil
}

// WriteCluster adds cluster index of device dev. data is ClusterSize bytes,
// the device's tail length for its last cluster, or empty for a zero cluster.
// An all-zero cluster is encoded with mask 0; all-zero 4 KiB blocks are left out.
func (w *Writer) WriteCluster(dev uint8, index uint32, data []byte) error {
	if err := w.usable(); err != nil {
		return err
	}
	d := w.devs[dev]
	if d == nil {
		return fmt.Errorf("vma: cluster for unknown device %d", dev)
	}
	if uint64(index) >= d.clusters {
		return fmt.Errorf("vma: device %d: cluster %d out of range (%d clusters)", dev, index, d.clusters)
	}
	if n := len(data); n != 0 && n != ClusterSize {
		tail := d.size - uint64(index)*ClusterSize
		if uint64(index) != d.clusters-1 || uint64(n) != tail {
			return fmt.Errorf("vma: device %d cluster %d: %d data bytes", dev, index, n)
		}
	}
	if d.seen[index/64]&(1<<(index%64)) != 0 {
		return fmt.Errorf("vma: device %d: duplicate cluster %d", dev, index)
	}
	if len(w.infos) == WriterExtentClusters {
		if err := w.flush(); err != nil {
			return err
		}
	}
	var mask uint16
	for i := 0; i < BlocksPerCluster; i++ {
		lo := i * BlockSize
		if lo >= len(data) {
			break
		}
		block := data[lo:min(lo+BlockSize, len(data))]
		if isZero(block) {
			continue
		}
		mask |= 1 << i
		w.data = append(w.data, block...)
		if pad := BlockSize - len(block); pad > 0 {
			w.data = append(w.data, make([]byte, pad)...)
		}
	}
	w.infos = append(w.infos, BlockInfo(mask, dev, index))
	d.seen[index/64] |= 1 << (index % 64)
	d.written++
	return nil
}

func isZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// flush writes the pending extent.
func (w *Writer) flush() error {
	h := make([]byte, ExtentHeaderSize, ExtentHeaderSize+len(w.data))
	be := binary.BigEndian
	copy(h[OffExtMagic:], ExtentMagic[:])
	be.PutUint16(h[OffExtBlockCount:], uint16(len(w.data)/BlockSize))
	copy(h[OffExtUUID:], w.uuid[:])
	for i, info := range w.infos {
		be.PutUint64(h[OffExtBlockInfo+8*i:], info)
	}
	sum := md5.Sum(h)
	copy(h[OffExtMD5:], sum[:])
	_, err := w.w.Write(append(h, w.data...))
	w.infos = w.infos[:0]
	w.data = w.data[:0]
	if err != nil {
		w.err = fmt.Errorf("vma: write extent: %w", err)
		return w.err
	}
	return nil
}

// Commit flushes the held-back final extent. It writes nothing and refuses
// while any device cluster is missing. Call it only after every data check
// has passed. Afterwards the writer accepts no more clusters.
func (w *Writer) Commit() error {
	if err := w.usable(); err != nil {
		return err
	}
	for id, d := range w.devs {
		if d != nil && d.written != d.clusters {
			return fmt.Errorf("vma: device %d: %d of %d clusters missing", id, d.clusters-d.written, d.clusters)
		}
	}
	if len(w.infos) > 0 {
		if err := w.flush(); err != nil {
			return err
		}
	}
	w.state = stateCommitted
	return nil
}

// Abort drops the held-back final extent. The stream written so far stays
// truncated; afterwards the writer accepts no more calls.
func (w *Writer) Abort() {
	if w.state == stateOpen {
		w.state = stateAborted
	}
	w.infos = w.infos[:0]
	w.data = w.data[:0]
}
