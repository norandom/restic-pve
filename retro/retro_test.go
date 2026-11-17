package retro

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

// sim returns an initialised simulation screen of cols x rows in a character set.
func sim(t *testing.T, charset string, cols, rows int) tcell.SimulationScreen {
	t.Helper()
	s := tcell.NewSimulationScreen(charset)
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	s.SetSize(cols, rows)
	t.Cleanup(s.Fini)
	return s
}

// text returns the screen as lines, and the style of each cell.
func text(s tcell.SimulationScreen) ([]string, func(x, y int) tcell.Style) {
	s.Show()
	cells, w, h := s.GetContents()
	lines := make([]string, h)
	for y := 0; y < h; y++ {
		var b strings.Builder
		for x := 0; x < w; x++ {
			if r := cells[y*w+x].Runes; len(r) > 0 {
				b.WriteRune(r[0])
			} else {
				b.WriteByte(' ')
			}
		}
		lines[y] = b.String()
	}
	return lines, func(x, y int) tcell.Style { return cells[y*w+x].Style }
}

func TestThemePalettes(t *testing.T) {
	cases := []struct {
		colors          int
		charset         string
		color           bool
		back, highlight tcell.Color
		ascii           bool
	}{
		{1 << 24, "UTF-8", true, rgbBlue, rgbYellow, false},
		{256, "utf8", true, rgbBlue, rgbYellow, false},
		{16, "UTF-8", true, tcell.ColorBlue, tcell.ColorYellow, false},
		{8, "UTF-8", true, tcell.ColorNavy, tcell.ColorOlive, false},
		{256, "US-ASCII", true, rgbBlue, rgbYellow, true},
		{256, "UTF-8", false, tcell.ColorDefault, tcell.ColorDefault, false},
		{0, "ISO8859-1", true, tcell.ColorDefault, tcell.ColorDefault, true},
	}
	for _, c := range cases {
		th := themeFor(c.colors, c.charset, c.color)
		_, back, _ := th.Base.Decompose()
		_, hl, _ := th.Focus.Decompose()
		if back != c.back || hl != c.highlight || th.ASCII != c.ascii {
			t.Errorf("%d %s %v: back %v highlight %v ascii %v", c.colors, c.charset, c.color, back, hl, th.ASCII)
		}
		if !th.Color {
			if _, _, a := th.Focus.Decompose(); a&tcell.AttrReverse == 0 {
				t.Errorf("%d %s %v: focus not inverse without colour", c.colors, c.charset, c.color)
			}
		}
	}
	// with 8 colours the yellow and white are made bright with bold
	if _, _, a := themeFor(8, "UTF-8", true).Title.Decompose(); a&tcell.AttrBold == 0 {
		t.Error("8 colours: title not bold")
	}
}

func TestWrapCutWidth(t *testing.T) {
	if got := strings.Join(Wrap("one two three four five", 9), "|"); got != "one two|three|four five" {
		t.Errorf("wrap %q", got)
	}
	if got := strings.Join(Wrap("FLEECING_ABORT_FREE_GIB must be", 10), "|"); got != "FLEECING_A|BORT_FREE_|GIB must|be" {
		t.Errorf("long word %q", got)
	}
	if Wrap("x", 0) != nil || Cut("abcdef", 3) != "abc" || Cut("ab", 3) != "ab" || Width("░◘─") != 3 {
		t.Error("cut or width")
	}
}

func TestBarsAndBoxes(t *testing.T) {
	for _, cs := range []string{"UTF-8", "US-ASCII"} {
		s := sim(t, cs, 40, 8)
		th := NewTheme(s, true)
		th.Apply()
		th.Background(s)
		th.TitleBar(s, 0, 40, "Title · here", "q=Quit")
		b := th.Box("Status")
		b.SetRect(1, 1, 20, 4)
		b.Draw(s)
		th.Progress(s, 2, 5, 10, 0.3)
		th.BottomBar(s, 7, 40, "Reading…", th.BarMsg, "tool")
		lines, style := text(s)
		want := []string{
			" Title · here                    q=Quit ",
			" ┌───── Status ─────┐                   ",
			" │                  │                   ",
			" │                  │                   ",
			" └──────────────────┘                   ",
			"  ███░░░░░░░                            ",
			"                                        ",
			"Reading…                         │ tool ",
		}
		if cs != "UTF-8" {
			r := strings.NewReplacer("┌", "+", "┐", "+", "└", "+", "┘", "+", "─", "-", "│", "|", "█", "#", "░", ".", "·", "-", "…", "...")
			for i := range want {
				want[i] = r.Replace(want[i])
			}
			want[7] = "Reading...                       | tool "
		}
		for i := range want {
			if lines[i] != want[i] {
				t.Errorf("%s line %d:\n got %q\nwant %q", cs, i, lines[i], want[i])
			}
		}
		if fg, bg, _ := style(1, 0).Decompose(); fg != rgbBlack || bg != rgbGrey {
			t.Errorf("title bar %v on %v", fg, bg)
		}
		if fg, bg, _ := style(33, 0).Decompose(); fg != rgbWhite || bg != rgbBlack {
			t.Errorf("key label %v on %v", fg, bg)
		}
		if fg, _, _ := style(8, 1).Decompose(); fg != rgbYellow {
			t.Errorf("box title %v", fg)
		}
		if fg, bg, _ := style(1, 1).Decompose(); fg != rgbWhite || bg != rgbBlue {
			t.Errorf("frame %v on %v", fg, bg)
		}
		if fg, _, _ := style(36, 7).Decompose(); fg != rgbRed {
			t.Errorf("bottom bar name %v", fg)
		}
	}
	// no colour: the same characters, default colours
	s := sim(t, "UTF-8", 40, 3)
	th := NewTheme(s, false)
	th.Apply()
	th.TitleBar(s, 0, 40, "Title", "q=Quit")
	lines, style := text(s)
	if lines[0] != " Title                           q=Quit " {
		t.Errorf("%q", lines[0])
	}
	if fg, bg, a := style(1, 0).Decompose(); fg != tcell.ColorDefault || bg != tcell.ColorDefault || a&tcell.AttrReverse == 0 {
		t.Errorf("no colour title bar %v %v %v", fg, bg, a)
	}
}

func TestBlockMapStatusLegend(t *testing.T) {
	s := sim(t, "UTF-8", 40, 16)
	th := NewTheme(s, true)
	th.Apply()
	m := NewBlockMap(th, 0)
	m.Cells = []State{Done, Done, DoneEmpty, Writing, Reading, Data, Empty, Data, Empty, Empty, Data, Empty}
	m.SetRect(1, 0, 5, 4)
	m.Draw(s)
	st := NewStatusBox(th)
	st.Block, st.Percent, st.Elapsed, st.Mode = 589, 50, 22*time.Second+time.Hour, "Full backup"
	st.SetRect(0, 4, 30, LowerHeight)
	st.Draw(s)
	lg := NewLegendBox(th, []LegendEntry{{Data, "Has data"}, {Empty, "Empty"}, {Reading, "Reading"}, {Writing, "Writing"}, {Done, "In backup"}},
		"Disk:  1 block = 64 MiB")
	lg.SetRect(0, 10, 40, LowerHeight)
	lg.Draw(s)
	lines, style := text(s)
	want := []string{
		" ◘◘░Wr",
		" ◘░◘░░",
		" ◘░   ",
		"      ",
		"┌────────── Status ──────────┐",
		"│  Block 589            50%  │",
		"│  ████████████░░░░░░░░░░░░  │",
		"│   Elapsed Time: 01:00:22   │",
		"│        Full backup         │",
		"└────────────────────────────┘",
		"┌─────────────── Legend ───────────────┐",
		"│  ◘ - Has data      ░ - Empty         │",
		"│  r - Reading       W - Writing       │",
		"│  ◘ - In backup                       │",
		"│  Disk:  1 block = 64 MiB             │",
		"└──────────────────────────────────────┘",
	}
	for i := range want {
		if !strings.HasPrefix(lines[i], want[i]) {
			t.Errorf("line %d:\n got %q\nwant %q", i, lines[i], want[i])
		}
	}
	yellow := func(x, y int) bool { fg, _, _ := style(x, y).Decompose(); return fg == rgbYellow }
	white := func(x, y int) bool { fg, _, _ := style(x, y).Decompose(); return fg == rgbWhite }
	if !yellow(1, 0) || !yellow(3, 0) || !white(4, 0) || !white(5, 0) || !white(1, 1) || !white(2, 1) || !yellow(3, 13) || !white(3, 11) {
		t.Error("cell colours")
	}
	// without colour a done block is a full block
	if r, _ := themeFor(256, "UTF-8", false).Cell(Done); r != '█' {
		t.Errorf("done without colour %q", r)
	}
	if r, _ := themeFor(256, "C", true).Cell(Data); r != 'o' {
		t.Errorf("data in ASCII %q", r)
	}
	if Elapsed(3*time.Hour+4*time.Minute+5*time.Second) != "03:04:05" {
		t.Error("elapsed")
	}
}

func TestLayout(t *testing.T) {
	l := NewLayout(80, 25)
	if l.BodyY != 1 || l.BodyH != 17 || l.LowerY != 18 || l.BottomBarY != 24 || l.LeftW != 38 || l.RightX != 40 || l.RightW() != 39 {
		t.Errorf("%+v %d", l, l.RightW())
	}
}
