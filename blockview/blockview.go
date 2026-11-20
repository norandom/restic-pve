// Package blockview is the packed block view of a backup: the model that turns what is known
// about every block of a disk into the cells of the map, and the screen that draws the map with
// its Status and Legend boxes in the retro house style (package retro).
//
// The demo ("restic-pve demo pack") and the live view of a real guest backup on the backup
// machine both draw through this package, so they cannot drift apart. docs/reference/house-style.md
// describes the view.
//
// The packed picture: the backup keeps only the blocks that hold data and stores them packed.
// Blocks whose data is in the backup form one solid yellow run from the top-left; the run is
// as long as the number of those blocks. Every other block is drawn at its own place: white
// when it holds data that is not in the backup yet, shaded when it is empty or its data has
// moved into the run, a quiet dot when it has not been read yet. "r" marks the blocks being
// read, "W" the end of the run while data goes into the backup.
package blockview

import (
	"slices"

	"github.com/norandom/restic-pve/retro"
)

// Cell is what is known about one block of the disk.
type Cell uint8

const (
	Unread Cell = iota // not read yet in this run
	Empty              // read: it holds no data
	Data               // holds data that is not in the backup yet
	Handed             // read, and all its data is in the backup
	Kept               // unchanged since the last backup: in the backup without being read
)

// Input is the state of a run: one Cell per block, and what is happening now.
type Input struct {
	Cells []Cell
	// Prior is optional (nil or len(Cells)): Prior[i] is true when the last backup found data
	// in block i. An Unread block with prior data is drawn as a block with data: that is the
	// last known layout, until the read passes and corrects it.
	Prior []bool
	// Reading lists the blocks being read now.
	Reading []int
	// Writing is the number of cells at the end of the run that show "W" (0: nothing is
	// being written now).
	Writing int
}

// Run is the length of the yellow run: the number of blocks whose data is in the backup,
// read in this run or kept from the last one.
func (in Input) Run() int {
	n := 0
	for _, c := range in.Cells {
		if c == Handed || c == Kept {
			n++
		}
	}
	return n
}

// Place returns the cells of a map drawn in place, for a probe: Proxmox's record is shown
// where it lies on the disk, nothing moves. A block marked changed (or of a disk without a
// record) is a block the next run reads (white); an unchanged one is shaded yellow.
func Place(in Input) []retro.State {
	cells := make([]retro.State, len(in.Cells))
	for i, c := range in.Cells {
		if c == Kept {
			cells[i] = retro.DoneEmpty
		} else {
			cells[i] = retro.Data
		}
	}
	return cells
}

// Pack returns the cells of the map and the length of the run.
//
// Cells [0, run) are the run (yellow). After it every block is drawn at its place: Data white,
// Empty and Handed (its data moved into the run) shaded, Unread as a block with data when
// Prior says so and as "not read yet" otherwise. "r" goes on the blocks being read that lie
// after the run, "W" on the Writing cells right after the end of the run; "W" wins where both
// fall on one cell. The run never has holes: nothing is drawn over it.
func Pack(in Input) (cells []retro.State, run int) {
	n := len(in.Cells)
	cells = make([]retro.State, n)
	run = in.Run()
	for i, c := range in.Cells {
		switch {
		case i < run:
			cells[i] = retro.Done
		case c == Data:
			cells[i] = retro.Data
		case c == Unread && i < len(in.Prior) && in.Prior[i]:
			cells[i] = retro.Data
		case c == Unread:
			cells[i] = retro.Unread
		default:
			cells[i] = retro.Empty
		}
	}
	for _, j := range in.Reading {
		if j >= run && j < n {
			cells[j] = retro.Reading
		}
	}
	for k := 0; k < in.Writing && run+k < n; k++ {
		cells[run+k] = retro.Writing
	}
	return cells, run
}

// Group merges every k neighbouring blocks into one, so that a map of many blocks fits a
// screen of at most limit cells; it returns the grouped input and k. A group is:
// not read yet when all its blocks are; holding data when one of them holds data that is not
// in the backup yet, or when it is partly read and some data is already in the backup; not
// read yet when it is partly read and nothing in it held data so far; kept when all its blocks
// are; in the backup when the rest is empty (kept blocks count as in the backup); empty
// otherwise. A group is being read when one of its blocks is, and had data
// last time when one of its blocks had.
func Group(in Input, limit int) (Input, int) {
	n := len(in.Cells)
	if limit < 1 || n <= limit {
		return in, 1
	}
	k := (n + limit - 1) / limit
	g := (n + k - 1) / k
	out := Input{Cells: make([]Cell, g), Writing: in.Writing}
	if len(in.Prior) > 0 {
		out.Prior = make([]bool, g)
	}
	for gi := range g {
		var unread, data, handed, kept int
		lo, hi := gi*k, min((gi+1)*k, n)
		for i := lo; i < hi; i++ {
			switch in.Cells[i] {
			case Unread:
				unread++
			case Data:
				data++
			case Handed:
				handed++
			case Kept:
				kept++
			}
			if i < len(in.Prior) && in.Prior[i] {
				out.Prior[gi] = true
			}
		}
		switch {
		case kept == hi-lo:
			out.Cells[gi] = Kept
		case unread == hi-lo:
			out.Cells[gi] = Unread
		case data > 0:
			out.Cells[gi] = Data
		case unread > 0 && handed+kept > 0:
			out.Cells[gi] = Data
		case unread > 0:
			out.Cells[gi] = Unread
		case handed+kept > 0:
			out.Cells[gi] = Handed
		default:
			out.Cells[gi] = Empty
		}
	}
	for _, j := range in.Reading {
		if j >= 0 && j < n && !slices.Contains(out.Reading, j/k) {
			out.Reading = append(out.Reading, j/k)
		}
	}
	return out, k
}
