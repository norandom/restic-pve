package demo

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/norandom/restic-pve/retro"
	"github.com/rivo/tview"
)

func count(cells []retro.State, s retro.State) int {
	n := 0
	for _, c := range cells {
		if c == s {
			n++
		}
	}
	return n
}

// checkPacked checks the invariant of a packed run: the run [0, Packed) is yellow without a
// hole, and nothing after it is yellow.
func checkPacked(t *testing.T, s *PackSim) {
	t.Helper()
	for i, c := range s.Cells {
		if (i < s.Packed()) != (c == retro.Done) {
			t.Fatalf("tick %d: block %d is %d, run %d", s.ticks, i, c, s.Packed())
		}
	}
}

func TestPackStart(t *testing.T) {
	s := NewPackSim(Full, MapSize(80, 25), PackSeed)
	if len(s.Cells) != 1320 || s.BlockMiB != 97 {
		t.Fatalf("%d blocks of %d MiB", len(s.Cells), s.BlockMiB)
	}
	// data scattered among empty blocks, dense in the first two fifths, sparse after them
	if d := count(s.Cells, retro.Data); d != s.Data || d+count(s.Cells, retro.Empty) != len(s.Cells) || d < 150 || d > 450 {
		t.Fatalf("%d blocks with data (%d)", d, s.Data)
	}
	if head, tail := count(s.Cells[:528], retro.Data), count(s.Cells[528:], retro.Data); head < 2*tail {
		t.Errorf("first two fifths %d, rest %d", head, tail)
	}
	if s.Packed() != 0 || s.Percent() != 0 || s.Finished() {
		t.Error("not at the start")
	}
	// the same size and seed give the same disk; another seed another one
	if !slices.Equal(s.Cells, NewPackSim(Full, MapSize(80, 25), PackSeed).Cells) || slices.Equal(s.Cells, NewPackSim(Full, MapSize(80, 25), 7).Cells) {
		t.Error("layout not deterministic by seed")
	}
}

func TestPackSteps(t *testing.T) {
	s := NewPackSim(Full, MapSize(80, 25), PackSeed)
	data := slices.Clone(s.Cells)
	// blocks that already sit at the end of the run turn yellow in place, one per tick
	inPlace := 0
	for data[inPlace] == retro.Data {
		inPlace++
	}
	for i := 0; i < inPlace; i++ {
		s.Step()
		checkPacked(t, s)
		if s.Packed() != i+1 || count(s.Cells, retro.Reading)+count(s.Cells, retro.Writing) != 0 {
			t.Fatalf("in place %d: run %d", i, s.Packed())
		}
	}
	// the first scattered block: "r" on it, then "W" at the end of the run, then the end of
	// the run is yellow and the block is empty; each shown for four ticks at the start
	src := inPlace + 1
	for data[src] != retro.Data {
		src++
	}
	var seen strings.Builder
	for s.Packed() == inPlace {
		s.Step()
		seen.WriteRune(map[retro.State]rune{retro.Data: 'o', retro.Reading: 'r', retro.Empty: '.', retro.Done: 'D'}[s.Cells[src]])
		seen.WriteRune(map[retro.State]rune{retro.Empty: '.', retro.Writing: 'W', retro.Done: 'D'}[s.Cells[inPlace]])
		seen.WriteByte(' ')
		if n := count(s.Cells, retro.Reading); n > 1 {
			t.Fatalf("%d blocks read", n)
		}
	}
	if got := seen.String(); got != "r. r. r. r. rW rW rW rW .D " {
		t.Errorf("source and run end per tick: %q", got)
	}
	checkPacked(t, s)
	// in the same tick the next scattered block is read; it goes right behind the run
	next := src + 1
	for data[next] != retro.Data {
		next++
	}
	if s.Cells[next] != retro.Reading || count(s.Cells, retro.Reading) != 1 || s.Cells[inPlace+1] != retro.Empty {
		t.Errorf("next: %d at %d", s.Cells[next], next)
	}
}

func TestPackRun(t *testing.T) {
	for _, size := range [][2]int{{80, 25}, {60, 20}, {200, 60}} {
		s := NewPackSim(Full, MapSize(size[0], size[1]), PackSeed)
		order := []int{}
		for !s.Finished() {
			s.Step()
			checkPacked(t, s)
			if r, w := count(s.Cells, retro.Reading), count(s.Cells, retro.Writing); s.Writing() && w == 0 || !s.Writing() && w > 0 || r+w > 40 {
				t.Fatalf("tick %d: %d reading, %d writing", s.ticks, r, w)
			}
			if len(order) == 0 || order[len(order)-1] != s.Packed() {
				order = append(order, s.Packed())
			}
			if s.ticks > 100000 {
				t.Fatal("no end")
			}
		}
		// the end: one solid yellow run of all blocks with data at the top, the rest empty
		if count(s.Cells[:s.Data], retro.Done) != s.Data || count(s.Cells[s.Data:], retro.Empty) != len(s.Cells)-s.Data || s.Percent() != 100 {
			t.Fatalf("%v: end state, run %d of %d", size, s.Packed(), s.Data)
		}
		if !slices.IsSorted(order) {
			t.Error("the run shrank")
		}
		// watchable: about 30 to 60 seconds, slow at the start
		d := time.Duration(s.ticks) * Tick
		t.Logf("%dx%d: %d blocks, %d with data, %d moved, %d ticks = %v, last level %v", size[0], size[1], len(s.Cells), s.Data, s.moved, s.ticks, d, packLevels[s.level])
		if lo := map[bool]time.Duration{true: 20 * time.Second, false: 30 * time.Second}[size[0] < 80]; d < lo || d > 60*time.Second {
			t.Errorf("%v: takes %v", size, d)
		}
		s.Step()
		if s.ticks > int(d/Tick) {
			t.Error("a finished run goes on")
		}
	}
}

func TestPackIncremental(t *testing.T) {
	full := NewPackSim(Full, MapSize(80, 25), PackSeed)
	s := NewPackSim(Incremental, MapSize(80, 25), PackSeed)
	changed := 0
	for i, c := range s.Cells {
		if c == retro.Data && s.changed[i] {
			changed++
		}
	}
	if changed == 0 || changed*4 > s.Data {
		t.Fatalf("%d of %d blocks changed", changed, s.Data)
	}
	// only changed blocks are read and written
	read := map[int]bool{}
	for !s.Finished() {
		s.Step()
		checkPacked(t, s)
		for i, c := range s.Cells {
			if c == retro.Reading {
				if !s.changed[i] {
					t.Fatalf("unchanged block %d read", i)
				}
				read[i] = true
			}
		}
		if s.ticks > 100000 {
			t.Fatal("no end")
		}
	}
	if len(read) != changed || s.moved > changed {
		t.Errorf("%d read, %d moved, %d changed", len(read), s.moved, changed)
	}
	if count(s.Cells[:s.Data], retro.Done) != s.Data || count(s.Cells[s.Data:], retro.Empty) != len(s.Cells)-s.Data {
		t.Error("end state")
	}
	for !full.Finished() {
		full.Step()
	}
	t.Logf("incremental %d ticks = %v, full %d ticks", s.ticks, time.Duration(s.ticks)*Tick, full.ticks)
	if s.ticks*3 > full.ticks {
		t.Errorf("incremental %d ticks, full %d", s.ticks, full.ticks)
	}
}

func TestPackView(t *testing.T) {
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	defer scr.Fini()
	scr.SetSize(80, 25)
	th := retro.NewTheme(scr, true)
	th.Apply()
	v := &View{Box: tview.NewBox(), T: th, Pack: NewPackSim(Full, MapSize(80, 25), PackSeed)}
	lines := func() []string {
		v.Draw(scr)
		scr.Show()
		cells, w, h := scr.GetContents()
		out := make([]string, h)
		for y := range h {
			var b strings.Builder
			for x := 0; x < w; x++ {
				b.WriteRune(cells[y*w+x].Runes[0])
			}
			out[y] = b.String()
		}
		return out
	}
	for range 300 {
		v.Pack.Step()
	}
	l := lines()
	if !strings.HasPrefix(l[0], " Demo · packed full backup · 125 GiB disk · reads no data") || !strings.Contains(l[19], "Block ") ||
		!strings.Contains(l[22], "Full backup · packed view") || !strings.Contains(l[22], "Data: ") || !strings.Contains(l[22], " blocks x 97 MiB") {
		for _, s := range l {
			t.Log(s)
		}
		t.Error("layout")
	}
	for !v.Pack.Finished() {
		v.Pack.Step()
	}
	v.Elapsed = 41 * time.Second
	l = lines()
	if !strings.Contains(l[19], "100%") || !strings.Contains(l[22], "Done in 00:00:41") || !strings.HasPrefix(l[24], "Finished. Press q to quit.") {
		for _, s := range l {
			t.Log(s)
		}
		t.Error("end")
	}
}
