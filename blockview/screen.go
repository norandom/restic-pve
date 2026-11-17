package blockview

import (
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/norandom/restic-pve/retro"
)

// Legend is what the cells of the packed view mean. The demo leaves out "Not read yet": its
// made-up disk is known from the start.
var Legend = []retro.LegendEntry{
	{State: retro.Data, Text: "Has data"}, {State: retro.Empty, Text: "Empty"},
	{State: retro.Reading, Text: "Reading"}, {State: retro.Writing, Text: "Writing"},
	{State: retro.Done, Text: "In backup"}, {State: retro.Unread, Text: "Not read yet"},
}

// MapSize is the number of map cells on a screen of cols x rows.
func MapSize(cols, rows int) int {
	l := retro.NewLayout(cols, rows)
	return max(cols-2, 1) * max(l.BodyH, 1)
}

// Screen is one full screen of the block view: the title bar, the map, the Status and Legend
// boxes and the bottom bar.
type Screen struct {
	Title   string // title bar, left
	Key     string // title bar, right (e.g. "q=Quit")
	Cells   []retro.State
	Block   int // Status line 1, left
	Percent int // Status line 1, right, and the progress track
	Elapsed time.Duration
	Mode    string // Status line 4: "Full backup", "Done in hh:mm:ss", ...
	Legend  []retro.LegendEntry
	Footer  string // the last line of the Legend box
	Msg     string // bottom bar, left, in the message colour
	Brand   string // bottom bar, right, after the separator
}

// Draw draws the screen on the whole of screen.
func (s Screen) Draw(screen tcell.Screen, t retro.Theme) {
	cols, rows := screen.Size()
	l := retro.NewLayout(cols, rows)
	t.Background(screen)
	t.TitleBar(screen, l.TitleBarY, cols, s.Title, s.Key)

	m := retro.NewBlockMap(t, 0)
	m.Cells = s.Cells
	m.SetRect(1, l.BodyY, cols-2, l.BodyH)
	m.Draw(screen)

	st := retro.NewStatusBox(t)
	st.Block, st.Percent, st.Elapsed, st.Mode = s.Block, s.Percent, s.Elapsed, s.Mode
	st.SetRect(1, l.LowerY, l.LeftW, retro.LowerHeight)
	st.Draw(screen)

	lg := retro.NewLegendBox(t, s.Legend, s.Footer)
	lg.SetRect(l.RightX, l.LowerY, l.RightW(), retro.LowerHeight)
	lg.Draw(screen)

	t.BottomBar(screen, l.BottomBarY, cols, s.Msg, t.BarMsg, s.Brand)
	screen.HideCursor()
}

// Notice is a full screen that only says something: no backup to show, how to start one.
type Notice struct {
	Title, Key string
	Heading    string   // title of the box
	Lines      []string // paragraphs, wrapped to the box
	Msg, Brand string   // bottom bar
}

// Draw draws the notice: the title bar, one framed box over the body with the paragraphs,
// and the bottom bar.
func (n Notice) Draw(screen tcell.Screen, t retro.Theme) {
	cols, rows := screen.Size()
	l := retro.NewLayout(cols, rows)
	t.Background(screen)
	t.TitleBar(screen, l.TitleBarY, cols, n.Title, n.Key)
	box := t.Box(n.Heading)
	h := rows - 2
	box.SetRect(1, l.BodyY, cols-2, h)
	box.Draw(screen)
	x, y, w, bh := box.GetInnerRect()
	x, w = x+2, w-4
	row := y + 1
	for i, p := range n.Lines {
		if i > 0 {
			row++
		}
		for _, line := range retro.Wrap(t.Text(p), w) {
			if row >= y+bh {
				break
			}
			retro.Put(screen, x, row, w, line, t.Base)
			row++
		}
	}
	t.BottomBar(screen, l.BottomBarY, cols, n.Msg, t.BarMsg, n.Brand)
	screen.HideCursor()
}
