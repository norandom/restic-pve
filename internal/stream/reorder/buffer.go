package reorder

import (
	"container/heap"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/norandom/restic-pve/internal/stream/vma"
)

// device is one layout device with its global position range and the bitmap
// of early zero clusters (1 bit per cluster, accounted in the RAM tier).
type device struct {
	info     vma.DevInfo
	clusters uint64
	start    uint64 // global position of cluster 0
	zero     []uint64
}

// entry is one buffered early cluster in RAM.
type entry struct {
	pos  uint64
	data []byte
	hi   int // index in the RAM heap
}

// farHeap is a max-heap on position: the root is the farthest buffered
// cluster in RAM.
type farHeap []*entry

func (h farHeap) Len() int           { return len(h) }
func (h farHeap) Less(i, j int) bool { return h[i].pos > h[j].pos }
func (h farHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].hi = i
	h[j].hi = j
}
func (h *farHeap) Push(x any) {
	e := x.(*entry)
	e.hi = len(*h)
	*h = append(*h, e)
}
func (h *farHeap) Pop() any {
	old := *h
	e := old[len(old)-1]
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	return e
}

// Buffer reorders clusters in RAM, then emits the farthest early clusters as
// patches when the RAM cap would be exceeded. The cursor is a global position:
// devices in Layout order, then cluster index. It is not safe for concurrent
// use.
//
// RAM accounting covers buffered cluster data (ClusterSize each), hash slots
// (HashSlotBytes each) and the zero bitmaps; index structures (maps, heaps,
// other metadata) and the single in-flight cluster are not counted.
type Buffer struct {
	caps Caps
	sink Sink
	hs   HashSink

	devs   []device
	byID   [256]int // device index + 1
	total  uint64
	cursor uint64
	curDev int

	entries map[uint64]*entry
	ram     farHeap
	hashes  map[uint64][32]byte

	ramData, hashBytes, bitmapBytes int64

	stats    Stats
	err      error // sticky
	finished bool
}

// BitmapBytes is the RAM the zero-cluster bitmaps of l take (the minimum
// RAMBytes New accepts).
func BitmapBytes(l Layout) int64 {
	var n int64
	for _, d := range l.Devices {
		n += int64((vma.Clusters(d.Size)+63)/64) * 8
	}
	return n
}

// New validates the RAM cap and layout. h may be nil.
func New(c Caps, l Layout, s Sink, h HashSink) (*Buffer, error) {
	if s == nil {
		return nil, errors.New("reorder: nil sink")
	}
	if len(l.Devices) == 0 {
		return nil, errors.New("reorder: layout has no devices")
	}
	if c.RAMBytes < 0 {
		return nil, fmt.Errorf("reorder: negative cap (ram %d)", c.RAMBytes)
	}
	b := &Buffer{caps: c, sink: s, hs: h, entries: map[uint64]*entry{}, hashes: map[uint64][32]byte{}}
	for i, d := range l.Devices {
		if i > 0 && d.ID <= l.Devices[i-1].ID {
			return nil, fmt.Errorf("reorder: layout devices not in ascending id order (%d after %d)", d.ID, l.Devices[i-1].ID)
		}
		n := vma.Clusters(d.Size)
		b.devs = append(b.devs, device{info: d, clusters: n, start: b.total, zero: make([]uint64, (n+63)/64)})
		b.byID[d.ID] = i + 1
		b.total += n
	}
	b.bitmapBytes = BitmapBytes(l)
	if b.bitmapBytes > c.RAMBytes {
		return nil, fmt.Errorf("reorder: RAM cap %d bytes is below the %d bytes of zero-cluster bitmaps", c.RAMBytes, b.bitmapBytes)
	}
	b.stats.RAMPeak = b.ramUsed()
	b.skipEmpty()
	return b, nil
}

func (b *Buffer) ramUsed() int64 { return b.ramData + b.hashBytes + b.bitmapBytes }

// Stats returns the peaks and counters so far and the current use.
func (b *Buffer) Stats() Stats {
	s := b.stats
	s.RAMInUse = b.ramUsed()
	return s
}

func (b *Buffer) fail(err error) error {
	if b.err == nil {
		b.err = err
	}
	return b.err
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidCluster, fmt.Sprintf(format, a...))
}

// Put accepts one cluster in arrival order. It takes ownership of c.Data
// (the vma.Reader yields a fresh slice per cluster). A cluster with Zero set
// or nil Data is a zero cluster.
func (b *Buffer) Put(c vma.Cluster) error {
	if b.err != nil {
		return b.err
	}
	if b.finished {
		return errors.New("reorder: Put after Finish")
	}
	di := b.byID[c.Dev]
	if di == 0 {
		return b.fail(invalid("device %d not in the layout", c.Dev))
	}
	d := &b.devs[di-1]
	if uint64(c.Index) >= d.clusters {
		return b.fail(invalid("device %d cluster %d out of range (%d clusters)", c.Dev, c.Index, d.clusters))
	}
	zero := c.Zero || c.Data == nil
	if !zero && len(c.Data) != ClusterSize {
		return b.fail(invalid("device %d cluster %d has %d data bytes", c.Dev, c.Index, len(c.Data)))
	}
	pos := d.start + uint64(c.Index)
	if pos < b.cursor || b.pending(d, pos) {
		return b.fail(invalid("device %d cluster %d arrived twice", c.Dev, c.Index))
	}
	var data []byte
	if !zero {
		data = c.Data
	}
	if pos == b.cursor {
		if err := b.sink.InOrder(c.Dev, c.Index, data); err != nil {
			return b.fail(err)
		}
		b.cursor++
		b.skipEmpty()
		if err := b.drain(); err != nil {
			return b.fail(err)
		}
		return nil
	}
	b.stats.EarlyClusters++
	if zero {
		i := pos - d.start
		d.zero[i/64] |= 1 << (i % 64)
		return nil
	}
	if err := b.store(pos, data); err != nil {
		return b.fail(err)
	}
	return nil
}

func (b *Buffer) pending(d *device, pos uint64) bool {
	i := pos - d.start
	if d.zero[i/64]&(1<<(i%64)) != 0 {
		return true
	}
	if _, ok := b.entries[pos]; ok {
		return true
	}
	_, ok := b.hashes[pos]
	return ok
}

// store buffers an early non-zero cluster in RAM, then patches the farthest
// of the buffered and arriving clusters when the RAM cap would be exceeded.
func (b *Buffer) store(pos uint64, data []byte) error {
	for {
		if b.ramUsed()+ClusterSize <= b.caps.RAMBytes {
			e := &entry{pos: pos, data: data}
			b.entries[pos] = e
			heap.Push(&b.ram, e)
			b.ramData += ClusterSize
			b.notePeaks()
			return nil
		}
		far := b.farthest()
		if far == nil || far.pos < pos {
			return b.patch(pos, data) // the arriving cluster is the farthest
		}
		// Evict the farthest buffered cluster as a patch, then retry:
		// RAM now has room (or the next iteration evicts again).
		fdata := b.take(far)
		if err := b.patch(far.pos, fdata); err != nil {
			return err
		}
	}
}

// farthest returns the buffered entry with the largest position, or nil.
func (b *Buffer) farthest() *entry {
	if len(b.ram) == 0 {
		return nil
	}
	return b.ram[0]
}

// take removes a buffered entry and returns its data.
func (b *Buffer) take(e *entry) []byte {
	delete(b.entries, e.pos)
	heap.Remove(&b.ram, e.hi)
	b.ramData -= ClusterSize
	return e.data
}

// patch writes a cluster ahead of the cursor and keeps its hash slot. Room
// for the hash slot is made by patching the farthest RAM-tier clusters (each
// frees ClusterSize for HashSlotBytes). If no RAM data is left, there is no
// room and ErrCapacity is returned.
func (b *Buffer) patch(pos uint64, data []byte) error {
	for b.ramUsed()+HashSlotBytes > b.caps.RAMBytes {
		if len(b.ram) == 0 {
			return fmt.Errorf("%w: %d hash slots outstanding, RAM cap %d bytes", ErrCapacity, b.hashBytes/HashSlotBytes, b.caps.RAMBytes)
		}
		e := b.ram[0]
		edata := b.take(e)
		if err := b.patch(e.pos, edata); err != nil {
			return err
		}
	}
	d, idx := b.locate(pos)
	n := min(uint64(ClusterSize), d.info.Size-idx*ClusterSize)
	sum := sha256.Sum256(data[:n])
	if err := b.sink.Patch(d.info.ID, uint32(idx), data); err != nil {
		return err
	}
	b.hashes[pos] = sum
	b.hashBytes += HashSlotBytes
	b.stats.PatchCount++
	b.notePeaks()
	return nil
}

// locate maps a global position to its device and cluster index.
func (b *Buffer) locate(pos uint64) (*device, uint64) {
	lo, hi := 0, len(b.devs)-1
	for lo < hi { // last device with start ≤ pos and clusters > 0
		mid := (lo + hi + 1) / 2
		if b.devs[mid].start <= pos {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	for b.devs[lo].clusters == 0 || pos >= b.devs[lo].start+b.devs[lo].clusters {
		lo++
	}
	return &b.devs[lo], pos - b.devs[lo].start
}

// skipEmpty moves curDev to the device that holds the cursor.
func (b *Buffer) skipEmpty() {
	for b.curDev < len(b.devs) && b.cursor >= b.devs[b.curDev].start+b.devs[b.curDev].clusters {
		b.curDev++
	}
}

// drain emits buffered successors at the cursor until a gap.
func (b *Buffer) drain() error {
	for b.cursor < b.total {
		d := &b.devs[b.curDev]
		i := b.cursor - d.start
		var data []byte
		switch {
		case d.zero[i/64]&(1<<(i%64)) != 0:
			d.zero[i/64] &^= 1 << (i % 64)
		default:
			if sum, ok := b.hashes[b.cursor]; ok {
				delete(b.hashes, b.cursor)
				b.hashBytes -= HashSlotBytes
				if b.hs != nil {
					b.hs.ClusterHash(d.info.ID, uint32(i), sum)
				}
				break
			}
			e, ok := b.entries[b.cursor]
			if !ok {
				return nil
			}
			data = b.take(e)
		}
		if err := b.sink.InOrder(d.info.ID, uint32(i), data); err != nil {
			return err
		}
		b.cursor++
		b.skipEmpty()
	}
	return nil
}

func (b *Buffer) notePeaks() {
	b.stats.RAMPeak = max(b.stats.RAMPeak, b.ramUsed())
}

// Finish reports missing clusters: every cluster
// of every device must have been put exactly once.
func (b *Buffer) Finish() error {
	b.finished = true
	if b.err != nil {
		return b.err
	}
	if b.cursor < b.total {
		pendingCount := uint64(len(b.entries) + len(b.hashes))
		for _, d := range b.devs {
			for _, w := range d.zero {
				for ; w != 0; w &= w - 1 {
					pendingCount++
				}
			}
		}
		d, idx := b.locate(b.cursor)
		return b.fail(fmt.Errorf("%w: %d missing, first at device %d cluster %d", ErrMissingCluster, b.total-b.cursor-pendingCount, d.info.ID, idx))
	}
	return nil
}
