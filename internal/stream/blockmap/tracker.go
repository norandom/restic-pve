package blockmap

import (
	"time"

	"github.com/norandom/restic-pve/internal/stream/vma"
)

// Tracker builds the map of a stream. Its methods are called from the stream goroutine only;
// they count into arrays the tracker owns and never block. At most every Interval it hands a
// snapshot to Publish (a non-blocking store).
//
// The disks of a guest are laid out one after the other in device order, as in the stream.
// Cell c covers the clusters [first(c), first(c+1)) with first(c) = ceil(c * clusters / cells).
type Tracker struct {
	base    State // VMID, Name, Mode, Started
	cells   int
	total   uint64 // clusters of all devices
	start   [256]uint64
	size    [256]uint64
	devOK   [256]bool
	frame   uint64 // clusters per data frame of the stream (the unit written to stdout)
	cellLen []uint32
	arrived []uint32
	data    []uint32
	handed  []uint32

	read, dataBytes, handedBytes uint64

	// data clusters passed in order but not yet written: they go out with their data frame
	pendCell  []int
	pendN     []uint32
	pendBytes uint64

	recent    []int // cells with arrivals since the last snapshot, newest last
	reading   []int
	now       func() time.Time
	next      time.Time
	publish   func(*State)
	finalized bool
}

// NewTracker returns a tracker for the devices of a stream; frameBytes is the size of the
// stream's data frames. publish receives every snapshot (it must not block); now may be nil.
func NewTracker(base State, devices []vma.DevInfo, frameBytes uint64, publish func(*State), now func() time.Time) *Tracker {
	if now == nil {
		now = time.Now
	}
	t := &Tracker{base: base, publish: publish, now: now, frame: max(frameBytes/vma.ClusterSize, 1)}
	for _, d := range devices {
		t.start[d.ID], t.size[d.ID], t.devOK[d.ID] = t.total, d.Size, true
		t.total += vma.Clusters(d.Size)
		t.base.Disk += d.Size
	}
	t.cells = int(min(t.total, MaxCells))
	t.cellLen = make([]uint32, t.cells)
	for c := range t.cells {
		t.cellLen[c] = uint32(t.first(c+1) - t.first(c))
	}
	t.arrived = make([]uint32, t.cells)
	t.data = make([]uint32, t.cells)
	t.handed = make([]uint32, t.cells)
	t.next = now()
	return t
}

func (t *Tracker) first(c int) uint64 {
	return (uint64(c)*t.total + uint64(t.cells) - 1) / uint64(t.cells)
}

// cell returns the cell and the byte length of a cluster; ok is false for a cluster outside
// the devices (the stream rejects it anyway).
func (t *Tracker) cell(dev uint8, index uint32) (c int, n uint64, ok bool) {
	lo := uint64(index) * vma.ClusterSize
	if !t.devOK[dev] || lo >= t.size[dev] || t.cells == 0 {
		return 0, 0, false
	}
	pos := t.start[dev] + uint64(index)
	return int(pos * uint64(t.cells) / t.total), min(vma.ClusterSize, t.size[dev]-lo), true
}

// Arrived records a cluster read from the source, in arrival order.
func (t *Tracker) Arrived(c vma.Cluster) {
	cell, n, ok := t.cell(c.Dev, c.Index)
	if !ok {
		return
	}
	t.arrived[cell]++
	t.read += n
	if !c.Zero && c.Data != nil {
		t.data[cell]++
		t.dataBytes += n
	}
	if k := len(t.recent); k == 0 || t.recent[k-1] != cell {
		if k == 8 {
			copy(t.recent, t.recent[1:])
			t.recent = t.recent[:7]
		}
		t.recent = append(t.recent, cell)
	}
	t.maybePublish()
}

// InOrder records that the stream passed a cluster to its data frame; hasData is false for a
// zero cluster and for a position written earlier as a patch. The data goes to stdout when the
// frame is complete: at a frame boundary or at the end of the device.
func (t *Tracker) InOrder(dev uint8, index uint32, hasData bool) {
	cell, n, ok := t.cell(dev, index)
	if !ok {
		return
	}
	if hasData {
		if k := len(t.pendCell); k > 0 && t.pendCell[k-1] == cell {
			t.pendN[k-1]++
		} else {
			t.pendCell = append(t.pendCell, cell)
			t.pendN = append(t.pendN, 1)
		}
		t.pendBytes += n
	}
	if (uint64(index)+1)%t.frame == 0 || (uint64(index)+1)*vma.ClusterSize >= t.size[dev] {
		for i, pc := range t.pendCell {
			t.handed[pc] += t.pendN[i]
		}
		t.handedBytes += t.pendBytes
		t.pendCell, t.pendN, t.pendBytes = t.pendCell[:0], t.pendN[:0], 0
	}
}

// Patch records a cluster written to stdout ahead of the cursor (a patch frame).
func (t *Tracker) Patch(dev uint8, index uint32) {
	cell, n, ok := t.cell(dev, index)
	if !ok {
		return
	}
	t.handed[cell]++
	t.handedBytes += n
}

func (t *Tracker) maybePublish() {
	if now := t.now(); !now.Before(t.next) {
		t.next = now.Add(Interval)
		t.publish(t.Snapshot(Running, ""))
	}
}

// Snapshot returns the current state. Reading lists the cells that had clusters arriving since
// the last snapshot and are not complete; without new arrivals it stays as it was.
func (t *Tracker) Snapshot(status, message string) *State {
	s := t.base
	s.Status, s.Message, s.Updated = status, message, t.now()
	s.Read, s.Data, s.Handed = t.read, t.dataBytes, t.handedBytes
	s.Map = make([]byte, t.cells)
	for c := range t.cells {
		switch {
		case t.arrived[c] == t.cellLen[c] && t.data[c] == 0:
			s.Map[c] = CellEmpty
		case t.data[c] == 0:
			s.Map[c] = CellUnread
		case t.arrived[c] == t.cellLen[c] && t.handed[c] >= t.data[c]:
			s.Map[c] = CellHanded
		default:
			s.Map[c] = CellData
		}
	}
	if len(t.recent) > 0 {
		t.reading = t.reading[:0]
		for _, c := range t.recent {
			if t.arrived[c] < t.cellLen[c] {
				t.reading = append(t.reading, c)
			}
		}
		t.recent = t.recent[:0]
	}
	if status == Running {
		s.Reading = append([]int(nil), t.reading...)
	}
	return &s
}

// Final publishes the last snapshot of the stream.
func (t *Tracker) Final(status, message string) *State {
	t.finalized = true
	s := t.Snapshot(status, message)
	t.publish(s)
	return s
}
