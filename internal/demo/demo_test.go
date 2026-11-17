package demo

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/norandom/restic-pve/retro"
	"github.com/rivo/tview"
)

// run steps a simulation to its end and returns the ticks it took and the order in which
// blocks turned done.
func run(t *testing.T, s *Sim) (ticks int, order []int) {
	t.Helper()
	seen := make([]bool, len(s.Cells))
	for !s.Finished() {
		s.Step()
		ticks++
		for i, c := range s.Cells {
			if (c == retro.Done || c == retro.DoneEmpty) && !seen[i] {
				seen[i] = true
				order = append(order, i)
			}
		}
		if ticks > 100000 {
			t.Fatal("no end")
		}
	}
	return ticks, order
}

func TestSimulation(t *testing.T) {
	full := NewSim(Full, MapSize(80, 25))
	if len(full.Cells) != 1320 || full.BlockMiB != 97 {
		t.Fatalf("%d blocks of %d MiB", len(full.Cells), full.BlockMiB)
	}
	data := 0
	for _, c := range full.Cells {
		if c == retro.Data {
			data++
		}
	}
	if data < len(full.Cells)/5 || data > len(full.Cells)*3/4 {
		t.Errorf("%d of %d blocks with data", data, len(full.Cells))
	}
	// the marker moves left to right, top to bottom: blocks turn done in order
	ticks, order := run(t, full)
	for i, b := range order {
		if b != i {
			t.Fatalf("block %d done as number %d", b, i)
		}
	}
	for i, c := range full.Cells {
		if want := map[bool]retro.State{true: retro.Done, false: retro.DoneEmpty}[full.data[i]]; c != want {
			t.Fatalf("block %d ends as %d", i, c)
		}
	}
	if full.Percent() != 100 {
		t.Error(full.Percent())
	}
	// an incremental run races over unchanged blocks and is much faster
	inc := NewSim(Incremental, MapSize(80, 25))
	incTicks, order := run(t, inc)
	if incTicks*3 > ticks {
		t.Errorf("incremental %d ticks, full %d", incTicks, ticks)
	}
	for i, b := range order {
		if b != i {
			t.Fatalf("incremental: block %d done as number %d", b, i)
		}
	}
	// it stays on a changed block: read, then written
	inc = NewSim(Incremental, MapSize(80, 25))
	var states []retro.State
	for !inc.Finished() && len(states) < 12 {
		inc.Step()
		if p := inc.Pos(); p < len(inc.Cells) && inc.changed[p] {
			states = append(states, inc.Cells[p])
		}
	}
	if s := states; len(s) < 6 || s[0] != retro.Reading || !strings.Contains(stateString(s), "rrrWWWW") {
		t.Errorf("changed block: %s", stateString(states))
	}
}

func stateString(s []retro.State) string {
	var b strings.Builder
	for _, c := range s {
		b.WriteRune(map[retro.State]rune{retro.Reading: 'r', retro.Writing: 'W', retro.Done: 'D', retro.Data: 'o', retro.Empty: '.', retro.DoneEmpty: ':'}[c])
	}
	return b.String()
}

func TestView(t *testing.T) {
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	defer scr.Fini()
	scr.SetSize(80, 25)
	th := retro.NewTheme(scr, true)
	th.Apply()
	v := &View{Box: tview.NewBox(), T: th, Sim: NewSim(Incremental, MapSize(80, 25))}
	for i := 0; i < 60; i++ {
		v.Sim.Step()
	}
	v.SetRect(0, 0, 80, 25)
	v.Draw(scr)
	scr.Show()
	cells, w, _ := scr.GetContents()
	line := func(y int) string {
		var b strings.Builder
		for x := 0; x < w; x++ {
			b.WriteRune(cells[y*w+x].Runes[0])
		}
		return b.String()
	}
	if !strings.HasPrefix(line(0), " Demo · simulated incremental backup · 125 GiB disk · reads no data") || !strings.HasSuffix(line(0), " q=Quit ") {
		t.Errorf("title %q", line(0))
	}
	if !strings.Contains(line(18), " Status ") || !strings.Contains(line(18), " Legend ") || !strings.Contains(line(22), "Incremental backup") ||
		!strings.Contains(line(22), "Demo disk:   1 block = 97 MiB") || !strings.Contains(line(19), "Block ") || !strings.HasSuffix(line(24), "│ restic-pve demo ") {
		for y := 0; y < 25; y++ {
			t.Log(line(y))
		}
		t.Error("layout")
	}
	// the map: one cell per block from the top-left, done ones yellow, the last row stops
	// where the disk ends
	if fg, _, _ := cells[w+1].Style.Decompose(); fg != tcell.NewRGBColor(0xff, 0xff, 0x55) {
		t.Errorf("first block not yellow: %v", fg)
	}
	n := len(v.Sim.Cells)
	lastRow, lastCol := 1+(n-1)/78, 1+(n-1)%78
	if r := cells[lastRow*w+lastCol].Runes[0]; r != '░' && r != '◘' {
		t.Errorf("last block %q", r)
	}
	if r := cells[lastRow*w+lastCol+1].Runes[0]; r != ' ' {
		t.Errorf("after the last block %q", r)
	}
}
