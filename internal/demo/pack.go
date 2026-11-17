package demo

import (
	"math/rand/v2"
	"time"

	"github.com/norandom/restic-pve/internal/retro"
)

// The packed view shows a backup the way the reference defragmenter moves blocks: the backup
// keeps only the blocks that hold data and stores them packed. The next scattered block with
// data is read ("r"), written ("W") to the end of a solid run that grows from the top-left,
// and its old place becomes empty. At the end one yellow run of all blocks with data stands at
// the top and the rest of the map is empty.

// PackSeed is the seed of the demo disk of the packed view.
const PackSeed = 1994

// PackTarget is about how long a full packed run takes, whatever the size of the map.
const PackTarget = 44 * time.Second

// packSlowStart is the number of moved blocks shown slowly, one at a time, before the run
// speeds up.
const packSlowStart = 12

// packLevel is one pace of a packed run: b blocks are read together for r ticks, then
// written together for w ticks.
type packLevel struct{ b, r, w int }

// packLevels go from slow to fast; a full run only ever moves one level up per batch.
var packLevels = []packLevel{{1, 4, 4}, {1, 4, 3}, {1, 3, 3}, {1, 3, 2}, {1, 2, 2}, {1, 2, 1}, {1, 1, 1},
	{2, 1, 1}, {3, 1, 1}, {4, 1, 1}, {6, 1, 1}, {8, 1, 1}, {12, 1, 1}, {16, 1, 1}, {24, 1, 1}, {32, 1, 1}}

// rate is blocks per tick.
func (l packLevel) rate() float64 { return float64(l.b) / float64(l.r+l.w) }

// packIncrementalPerTick is how many unchanged blocks join the run per tick in an incremental
// packed run; changed blocks are read and written at packIncremental.
const packIncrementalPerTick = 3

var packIncremental = packLevel{1, 4, 4}

type packPhase uint8

const (
	packIdle packPhase = iota
	packReading
	packWriting
)

// PackSim is a simulated packed run over a disk of blocks. Step moves it on by one tick.
type PackSim struct {
	Mode     Mode
	Cells    []retro.State
	BlockMiB int
	Data     int    // blocks with data: the length of the run at the end
	changed  []bool // the block changed since the last backup (incremental)
	run      int    // cells [0, run) are the packed run
	scan     int    // no block with data is left in [run, scan) outside the current batch
	phase    packPhase
	src      []int // the blocks read in the current batch, in order
	wait     int   // ticks left in the current phase
	ticks    int
	moved    int // blocks moved with "r" and "W"
	level    int
	pace     packLevel // of the current batch
}

// packLayout lays out n blocks for the packed view, like the reference picture: the first two
// fifths of the disk densely used, the rest sparse, mostly single blocks.
func packLayout(n int, seed uint64) (data, changed []bool) {
	data, changed = make([]bool, n), make([]bool, n)
	rng := rand.New(rand.NewPCG(seed, 6))
	in, ch := false, false
	for i := range n {
		p := 0.05
		if i*5 < n*2 {
			p = 0.34
		}
		if in {
			p += 0.08
		}
		in = rng.Float64() < p
		data[i] = in
		ch = in && (rng.Float64() < 0.06 || ch && rng.Float64() < 0.6)
		changed[i] = ch
	}
	return data, changed
}

// NewPackSim lays out a disk of DiskGiB over at most cells blocks; the layout depends only on
// the number of cells and the seed.
func NewPackSim(mode Mode, cells int, seed uint64) *PackSim {
	cells = max(cells, 1)
	total := DiskGiB * 1024
	blockMiB := (total + cells - 1) / cells
	n := (total + blockMiB - 1) / blockMiB
	data, changed := packLayout(n, seed)
	s := &PackSim{Mode: mode, Cells: make([]retro.State, n), BlockMiB: blockMiB, changed: changed}
	for i, d := range data {
		if d {
			s.Cells[i] = retro.Data
			s.Data++
		}
	}
	return s
}

// Packed is the number of blocks in the run.
func (s *PackSim) Packed() int { return s.run }

// Percent is the share of blocks with data in the run.
func (s *PackSim) Percent() int {
	if s.Data == 0 {
		return 100
	}
	return s.run * 100 / s.Data
}

// Finished reports whether every block with data is in the run.
func (s *PackSim) Finished() bool { return s.run >= s.Data && s.phase == packIdle }

// Writing reports whether blocks are being written now.
func (s *PackSim) Writing() bool { return s.phase == packWriting }

// next is the first block with data after the run that is not in the current batch; -1 when
// there is none.
func (s *PackSim) next() int {
	for s.scan = max(s.scan, s.run); s.scan < len(s.Cells); s.scan++ {
		if s.Cells[s.scan] == retro.Data {
			return s.scan
		}
	}
	return -1
}

// join puts block j at the end of the run without "r" and "W".
func (s *PackSim) join(j int) {
	s.Cells[j] = retro.Empty
	s.Cells[s.run] = retro.Done
	s.run++
}

// nextPace picks the level of the next batch of a full run: slow for the first blocks, then
// gradually faster, as fast as needed to end near PackTarget.
func (s *PackSim) nextPace() packLevel {
	if s.Mode == Incremental {
		return packIncremental
	}
	if s.moved >= packSlowStart && s.level+1 < len(packLevels) {
		left := max(int(PackTarget/Tick)-s.ticks, 1)
		if packLevels[s.level].rate() < float64(s.Data-s.run)/float64(left) {
			s.level++
		}
	}
	return packLevels[s.level]
}

// Step moves the run on by one tick.
//
// A full run: a block with data that already sits at the end of the run turns yellow in
// place (one per tick). Otherwise the next blocks with data are read ("r"), then the cells at
// the end of the run are written ("W"); then those cells turn yellow, the read blocks become
// empty and the next block is taken in the same tick.
//
// An incremental run: unchanged blocks join the run quickly, without "r" and "W"; only
// changed blocks are read and written.
func (s *PackSim) Step() {
	if s.Finished() {
		return
	}
	s.ticks++
	if s.wait > 0 {
		s.wait--
		return
	}
	switch s.phase {
	case packReading:
		for k := range s.src {
			s.Cells[s.run+k] = retro.Writing
		}
		s.phase = packWriting
		s.wait = s.pace.w - 1
		return
	case packWriting:
		for _, j := range s.src {
			s.Cells[j] = retro.Empty
		}
		for range s.src {
			s.Cells[s.run] = retro.Done
			s.run++
		}
		s.moved += len(s.src)
		s.src = s.src[:0]
		s.phase = packIdle
		// and on to the next blocks in the same tick: the work never pauses
	}
	quick := 1
	if s.Mode == Incremental {
		quick = packIncrementalPerTick
	}
	for n := 0; n < quick; n++ {
		j := s.next()
		switch {
		case j < 0:
			return
		case j == s.run, s.Mode == Incremental && !s.changed[j]:
			s.join(j)
		case n == 0:
			s.startBatch()
			return
		default:
			return
		}
	}
}

// startBatch reads the next blocks with data, as many as the pace allows.
func (s *PackSim) startBatch() {
	l := s.nextPace()
	s.pace = l
	for len(s.src) < l.b {
		j := s.next()
		if j < 0 || s.Mode == Incremental && len(s.src) > 0 {
			break
		}
		s.Cells[j] = retro.Reading
		s.src = append(s.src, j)
	}
	s.phase = packReading
	s.wait = l.r - 1
}
