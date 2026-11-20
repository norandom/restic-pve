package retro

import (
	"fmt"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// State is the state of one block of a disk on the block map.
type State uint8

const (
	Empty     State = iota // holds no data (shaded)
	Data                   // has data, not read yet (white block)
	Reading                // being read now ("r")
	Writing                // its new data goes into the backup now ("W")
	Done                   // read and in the backup (yellow block)
	DoneEmpty              // empty and passed (yellow shade)
	Unread                 // not read yet and nothing known about it (a quiet dot)
)

// Cell returns the character and style of a block in state s.
func (t Theme) Cell(s State) (rune, tcell.Style) {
	switch s {
	case Data:
		return t.G.Block, t.CellData
	case Reading:
		return 'r', t.CellWork
	case Writing:
		return 'W', t.CellWork
	case Done:
		if !t.Color {
			return t.G.Full, t.CellDone
		}
		return t.G.Block, t.CellDone
	case Unread:
		return t.G.Unread, t.CellEmpty
	case DoneEmpty:
		if !t.Color {
			return t.G.DoneShade, t.CellDone
		}
		return t.G.Shade, t.CellDone
	}
	return t.G.Shade, t.CellEmpty
}

// BlockMap is the map of a disk: one character cell per block, left to right, top to bottom,
// edge to edge without a frame. The last row stops where the disk ends.
type BlockMap struct {
	*tview.Box
	T     Theme
	Cells []State
}

// NewBlockMap returns a map of n empty blocks.
func NewBlockMap(t Theme, n int) *BlockMap {
	_, bg, _ := t.Base.Decompose()
	m := &BlockMap{Box: tview.NewBox().SetBackgroundColor(bg), T: t, Cells: make([]State, n)}
	if !t.Color {
		m.SetBackgroundColor(tcell.ColorDefault)
	}
	return m
}

// Draw draws the cells into the rectangle of the map; cells beyond it are not shown.
func (m *BlockMap) Draw(screen tcell.Screen) {
	m.DrawForSubclass(screen, m)
	x, y, w, h := m.GetInnerRect()
	if w <= 0 {
		return
	}
	for i, s := range m.Cells {
		row := i / w
		if row >= h {
			break
		}
		r, st := m.T.Cell(s)
		screen.SetContent(x+i%w, y+row, r, nil, st)
	}
}

// StatusBox is the left lower box of the block map: the block being worked on and the
// percentage, the progress track, the elapsed time and the kind of run.
type StatusBox struct {
	*tview.Box
	T       Theme
	Label   string // the word before the number on line 1; "" is "Block"
	Block   int
	Percent int
	Elapsed time.Duration
	Mode    string // "Full backup", "Incremental backup"
}

// NewStatusBox returns the box titled "Status".
func NewStatusBox(t Theme) *StatusBox { return &StatusBox{Box: t.Box("Status"), T: t} }

// Elapsed formats a duration as hh:mm:ss.
func Elapsed(d time.Duration) string {
	s := int(d / time.Second)
	return fmt.Sprintf("%02d:%02d:%02d", s/3600, s/60%60, s%60)
}

func (b *StatusBox) Draw(screen tcell.Screen) {
	b.DrawForSubclass(screen, b)
	x, y, w, h := b.GetInnerRect()
	x, w = x+2, w-4 // the reference keeps two columns free inside the frame
	if w <= 0 || h < 4 {
		return
	}
	label := b.Label
	if label == "" {
		label = "Block"
	}
	LeftRight(screen, x, y, w, fmt.Sprintf("%s %d", label, b.Block), fmt.Sprintf("%d%%", b.Percent), b.T.Base)
	b.T.Progress(screen, x, y+1, w, float64(b.Percent)/100)
	Center(screen, x, y+2, w, "Elapsed Time: "+Elapsed(b.Elapsed), b.T.Base)
	Center(screen, x, y+3, w, b.Mode, b.T.Base)
}

// LegendEntry is one entry of the legend: the cell as on the map and its meaning.
type LegendEntry struct {
	State State
	Text  string
}

// LegendBox is the right lower box of the block map: what each cell means, two entries per
// line, and a last line with the size of one block.
type LegendBox struct {
	*tview.Box
	T       Theme
	Entries []LegendEntry
	Footer  string // e.g. "Guest 100:   1 block = 64 MiB"
}

// NewLegendBox returns the box titled "Legend".
func NewLegendBox(t Theme, entries []LegendEntry, footer string) *LegendBox {
	return &LegendBox{Box: t.Box("Legend"), T: t, Entries: entries, Footer: footer}
}

func (b *LegendBox) Draw(screen tcell.Screen) {
	b.DrawForSubclass(screen, b)
	x, y, w, h := b.GetInnerRect()
	x, w = x+2, w-3
	if w <= 0 || h < 1 {
		return
	}
	half := (w + 1) / 2
	rows := min((len(b.Entries)+1)/2, h-1)
	for i, e := range b.Entries {
		row, col := i/2, i%2
		if row >= rows {
			break
		}
		cx := x + col*half
		r, st := b.T.Cell(e.State)
		screen.SetContent(cx, y+row, r, nil, st)
		Put(screen, cx+1, y+row, half-2, " - "+e.Text, b.T.Base)
	}
	if b.Footer != "" {
		Put(screen, x, y+h-1, w, b.Footer, b.T.Base)
	}
}
