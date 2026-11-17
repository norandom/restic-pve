package retro

import (
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"github.com/rivo/uniseg"
)

// LowerHeight is the height of the box row above the bottom bar (Status and Legend on the
// block map, Progress and Help on a form): a frame and four lines.
const LowerHeight = 6

// Layout is where the parts of a full screen go: the title bar on the first row, the body,
// the two lower boxes side by side, and the bottom bar on the last row.
type Layout struct {
	Cols, Rows  int
	BodyY       int // first row of the body
	BodyH       int // rows of the body
	LowerY      int // first row of the lower boxes
	LeftW       int // width of the left lower box; the right one takes the rest
	RightX      int // first column of the right lower box
	BottomBarY  int
	TitleBarY   int
	LowerBoxesH int
}

// NewLayout splits a screen of cols x rows the way the reference does: the lower boxes take
// one half each with a one-column gap, two columns in from the edges.
func NewLayout(cols, rows int) Layout {
	l := Layout{Cols: cols, Rows: rows, TitleBarY: 0, BodyY: 1, LowerBoxesH: LowerHeight}
	l.BottomBarY = rows - 1
	l.LowerY = rows - 1 - LowerHeight
	l.BodyH = max(l.LowerY-l.BodyY, 0)
	inner := cols - 2 // one column margin on each side
	l.LeftW = (inner - 1) / 2
	l.RightX = 1 + l.LeftW + 1
	return l
}

// RightW is the width of the right lower box.
func (l Layout) RightW() int { return l.Cols - 1 - l.RightX }

// Width is the number of columns s takes on a terminal.
func Width(s string) int { return uniseg.StringWidth(s) }

// Put writes s at x,y in style st, at most w columns, and returns the columns written.
func Put(screen tcell.Screen, x, y, w int, s string, st tcell.Style) int {
	n := 0
	for _, r := range s {
		rw := uniseg.StringWidth(string(r))
		if rw == 0 {
			continue
		}
		if n+rw > w {
			break
		}
		screen.SetContent(x+n, y, r, nil, st)
		n += rw
	}
	return n
}

// Fill paints w x h cells from x,y with r in style st.
func Fill(screen tcell.Screen, x, y, w, h int, r rune, st tcell.Style) {
	for row := y; row < y+h; row++ {
		for col := x; col < x+w; col++ {
			screen.SetContent(col, row, r, nil, st)
		}
	}
}

// Center writes s centred in w columns from x.
func Center(screen tcell.Screen, x, y, w int, s string, st tcell.Style) {
	sw := Width(s)
	Put(screen, x+max((w-sw)/2, 0), y, w, s, st)
}

// LeftRight writes left at x and right ending at x+w, right first so it always shows.
func LeftRight(screen tcell.Screen, x, y, w int, left, right string, st tcell.Style) {
	rw := Width(right)
	Put(screen, x+max(w-rw, 0), y, w, right, st)
	Put(screen, x, y, max(w-rw-1, 0), left, st)
}

// Cut shortens s to at most w columns.
func Cut(s string, w int) string {
	if Width(s) <= w {
		return s
	}
	var b strings.Builder
	n := 0
	for _, r := range s {
		rw := uniseg.StringWidth(string(r))
		if n+rw > w {
			break
		}
		b.WriteRune(r)
		n += rw
	}
	return b.String()
}

// Wrap breaks text into lines of at most w columns at spaces; a word longer than a line is
// broken where the line ends.
func Wrap(text string, w int) []string {
	if w < 1 {
		return nil
	}
	var lines []string
	line := ""
	for _, word := range strings.Fields(text) {
		for Width(word) > w {
			if line != "" {
				lines = append(lines, line)
				line = ""
			}
			head := Cut(word, w)
			lines = append(lines, head)
			word = word[len(head):]
		}
		switch {
		case word == "":
		case line == "":
			line = word
		case Width(line)+1+Width(word) <= w:
			line += " " + word
		default:
			lines = append(lines, line)
			line = word
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

// Text returns s as the terminal can show it: with ASCII glyphs the middle dot and the
// ellipsis become "-" and "...".
func (t Theme) Text(s string) string {
	if !t.ASCII {
		return s
	}
	return strings.NewReplacer("·", "-", "…", "...").Replace(s)
}

// Background paints the whole screen in the base style.
func (t Theme) Background(screen tcell.Screen) {
	cols, rows := screen.Size()
	Fill(screen, 0, 0, cols, rows, ' ', t.Base)
}

// TitleBar draws the grey bar on row y: left from the second column, and the key label right
// in inverse at the end when both fit.
func (t Theme) TitleBar(screen tcell.Screen, y, cols int, left, right string) {
	left, right = t.Text(left), t.Text(right)
	Fill(screen, 0, y, cols, 1, ' ', t.Bar)
	rw := 0
	if right != "" && Width(left)+Width(right)+4 <= cols {
		rw = Width(right) + 2
		Put(screen, cols-rw, y, rw, " "+right+" ", t.BarKey)
	}
	Put(screen, 1, y, cols-2-rw, left, t.Bar)
}

// BottomBar draws the grey message bar on row y: the message left in style st, and right
// after a separator in the message colour.
func (t Theme) BottomBar(screen tcell.Screen, y, cols int, msg string, st tcell.Style, right string) {
	msg, right = t.Text(msg), t.Text(right)
	Fill(screen, 0, y, cols, 1, ' ', t.Bar)
	rw := 0
	if right != "" && Width(msg)+Width(right)+5 <= cols {
		rw = Width(right) + 3
		x := cols - rw
		screen.SetContent(x, y, t.G.Sep, nil, t.Bar)
		Put(screen, x+2, y, rw-2, right, t.BarMsg)
	}
	Put(screen, 0, y, cols-rw, msg, st)
}

// Box is a framed box with its title centred in the top line, as a tview primitive. Its
// content is drawn by the function given to SetDrawFunc or by the embedding type.
func (t Theme) Box(title string) *tview.Box {
	_, bg, _ := t.Base.Decompose()
	titleFg, _, _ := t.Title.Decompose()
	b := tview.NewBox().SetBorder(true).SetBorderStyle(t.Frame).SetBackgroundColor(bg).SetTitleColor(titleFg)
	if !t.Color {
		b.SetBackgroundColor(tcell.ColorDefault).SetTitleColor(tcell.ColorDefault)
	}
	if title != "" {
		b.SetTitle(" " + tview.Escape(title) + " ")
	}
	return b
}

// Progress draws a progress track of w columns: the done part filled, the rest shaded.
func (t Theme) Progress(screen tcell.Screen, x, y, w int, done float64) {
	n := int(done*float64(w) + 0.5)
	n = min(max(n, 0), w)
	Fill(screen, x, y, n, 1, t.G.Full, t.TrackFull)
	Fill(screen, x+n, y, w-n, 1, t.G.Shade, t.Track)
}

// Checkbox is the mark of a ticked or unticked row.
func Checkbox(on bool) string {
	if on {
		return "[x]"
	}
	return "[ ]"
}

// Radio is the mark of the chosen or another row of a one-of list.
func Radio(on bool) string {
	if on {
		return "(*)"
	}
	return "( )"
}

// Button is the label of a button.
func Button(label string) string { return "[ " + label + " ]" }
