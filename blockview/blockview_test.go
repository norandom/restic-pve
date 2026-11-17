package blockview

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/norandom/restic-pve/retro"
)

// parse reads a map written as one character per block: '?' not read yet, '.' empty, 'o' data,
// '#' in the backup.
func parse(s string) []Cell {
	m := map[rune]Cell{'?': Unread, '.': Empty, 'o': Data, '#': Handed}
	out := make([]Cell, 0, len(s))
	for _, r := range s {
		out = append(out, m[r])
	}
	return out
}

// show writes the cells of a map as characters: 'D' run, 'o' data, '.' empty, '·' not read
// yet, 'r' reading, 'W' writing.
func show(cells []retro.State) string {
	m := map[retro.State]rune{retro.Done: 'D', retro.Data: 'o', retro.Empty: '.', retro.Unread: '·',
		retro.Reading: 'r', retro.Writing: 'W'}
	var b strings.Builder
	for _, c := range cells {
		b.WriteRune(m[c])
	}
	return b.String()
}

func TestPack(t *testing.T) {
	for _, tc := range []struct {
		name, cells string
		prior       []int
		reading     []int
		writing     int
		want        string
		run         int
	}{
		{"start, nothing known", "????????", nil, []int{0}, 0, "r·······", 0},
		{"start with the last layout", "????????", []int{1, 2, 6}, []int{0}, 0, "roo···o·", 0},
		{"first data read, being written", "oo.?????", nil, []int{3}, 1, "Wo.r····", 0},
		{"two blocks in the backup", "##.o.???", nil, []int{5}, 1, "DDWo.r··", 2},
		{"handed blocks after the run are empty", ".#.#o.??", []int{7}, []int{6}, 0, "DD..o.ro", 2},
		{"read ahead, not in the backup yet", "#..o?o??", nil, []int{4}, 0, "D..oro··", 1},
		{"reading inside the run is not drawn", "##o?", nil, []int{0, 3}, 0, "DDor", 2},
		{"W wins over r", "#.o??", nil, []int{1}, 1, "DWo··", 1},
		{"W past the end is dropped", "###", nil, nil, 2, "DDD", 3},
		{"done", ".#..##.#", nil, nil, 0, "DDDD....", 4},
	} {
		in := Input{Cells: parse(tc.cells), Reading: tc.reading, Writing: tc.writing}
		if tc.prior != nil {
			in.Prior = make([]bool, len(in.Cells))
			for _, i := range tc.prior {
				in.Prior[i] = true
			}
		}
		cells, run := Pack(in)
		if got := show(cells); got != tc.want || run != tc.run {
			t.Errorf("%s: %s run %d, want %s run %d", tc.name, got, run, tc.want, tc.run)
		}
	}
}

// TestPackInvariants runs a disk through random real-looking states: the run is as long as
// the blocks in the backup, has no holes, and nothing after it is yellow.
func TestPackInvariants(t *testing.T) {
	cells := parse(strings.Repeat("o..oo.o...ooo.....o.", 20))
	n := len(cells)
	state := make([]Cell, n)
	for cursor := 0; cursor <= n; cursor++ {
		for i := range n {
			switch {
			case i < cursor-3: // passed: in the backup or empty
				state[i] = map[bool]Cell{true: Handed, false: Empty}[cells[i] == Data]
			case i < cursor: // read, data not handed on yet
				state[i] = cells[i]
			default:
				state[i] = Unread
			}
		}
		in := Input{Cells: state, Reading: []int{cursor}, Writing: 1}
		out, run := Pack(in)
		handed := 0
		for _, c := range state {
			if c == Handed {
				handed++
			}
		}
		if run != handed {
			t.Fatalf("cursor %d: run %d, %d blocks in the backup", cursor, run, handed)
		}
		for i, c := range out {
			if (i < run) != (c == retro.Done) {
				t.Fatalf("cursor %d: %s", cursor, show(out))
			}
		}
		if run < n && out[run] != retro.Writing {
			t.Fatalf("cursor %d: no W at the end of the run: %s", cursor, show(out))
		}
		// every block with data not in the backup yet is still on the map
		if want := strings.Count(show(out), "o") + strings.Count(show(out), "r") + strings.Count(show(out), "W"); want < countData(state) {
			t.Fatalf("cursor %d: %d blocks with data drawn, %d known", cursor, want, countData(state))
		}
	}
}

func countData(cells []Cell) int {
	n := 0
	for _, c := range cells {
		if c == Data {
			n++
		}
	}
	return n
}

func TestGroup(t *testing.T) {
	in := Input{Cells: parse("##..##.." + "#.??" + "??..o.#." + "???." + "??????"), Reading: []int{9, 10, 11}, Writing: 1}
	g, k := Group(in, 8)
	if k != 4 || len(g.Cells) != 8 {
		t.Fatalf("k %d, %d groups", k, len(g.Cells))
	}
	// "##.." handed; "##.." handed; "#.??" partly read with data in the backup: data;
	// "??.." partly read, nothing so far: not read yet; "o.#." data; "???." not read yet;
	// "????" and "??" not read yet
	if got, want := g.Cells, []Cell{Handed, Handed, Data, Unread, Data, Unread, Unread, Unread}; !slices.Equal(got, want) {
		t.Errorf("groups %v, want %v", got, want)
	}
	if !slices.Equal(g.Reading, []int{2}) || g.Writing != 1 || g.Prior != nil {
		t.Errorf("reading %v writing %d prior %v", g.Reading, g.Writing, g.Prior)
	}
	in.Prior = make([]bool, len(in.Cells))
	in.Prior[25] = true
	if g, _ := Group(in, 8); !slices.Equal(g.Prior, []bool{false, false, false, false, false, false, true, false}) {
		t.Errorf("prior %v", g.Prior)
	}
	if same, k := Group(in, 100); k != 1 || len(same.Cells) != len(in.Cells) {
		t.Error("a map that fits is not grouped")
	}
}

func lines(s tcell.SimulationScreen) []string {
	s.Show()
	cells, w, h := s.GetContents()
	out := make([]string, h)
	for y := range h {
		var b strings.Builder
		for x := range w {
			b.WriteRune(cells[y*w+x].Runes[0])
		}
		out[y] = b.String()
	}
	return out
}

func simScreen(t *testing.T) tcell.SimulationScreen {
	t.Helper()
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(scr.Fini)
	scr.SetSize(80, 25)
	return scr
}

func TestScreenAndNotice(t *testing.T) {
	scr := simScreen(t)
	th := retro.NewTheme(scr, true)
	cells, run := Pack(Input{Cells: parse("##.o.??"), Reading: []int{5}, Writing: 1})
	Screen{Title: "Guest 100 · full backup", Key: "q=Quit", Cells: cells, Block: run, Percent: 42,
		Elapsed: 83 * time.Second, Mode: "Full backup", Legend: Legend, Footer: "Guest 100:  1 block = 32 MiB",
		Msg: "Reading...", Brand: "the backup goes on"}.Draw(scr, th)
	l := lines(scr)
	checks := []struct {
		row  int
		text string
	}{
		{0, " Guest 100 · full backup"}, {0, "q=Quit"}, {1, " DDWo.r·"}, {19, "Block 2"}, {19, "42%"},
		{21, "Elapsed Time: 00:01:23"}, {22, "Full backup"}, {19, "◘ - Has data"}, {20, "r - Reading"},
		{21, "· - Not read yet"}, {22, "Guest 100:  1 block = 32 MiB"}, {24, "Reading..."}, {24, "the backup goes on"},
	}
	for _, c := range checks {
		if c.row == 1 {
			// the map cells, drawn with the glyphs
			want := strings.NewReplacer("D", "◘", "o", "◘", ".", "░").Replace(c.text)
			if !strings.HasPrefix(l[1], want) {
				t.Errorf("map row %q, want %q", l[1], want)
			}
			continue
		}
		if !strings.Contains(l[c.row], c.text) {
			t.Errorf("row %d %q lacks %q", c.row, l[c.row], c.text)
		}
	}
	Notice{Title: "vmastream watch", Key: "q=Quit", Heading: "No backup running",
		Lines: []string{"No guest backup is running right now.", "Start one on the backup machine: ./backup.sh <guest>"},
		Msg:   "Waiting for a backup...", Brand: "vmastream"}.Draw(scr, th)
	l = lines(scr)
	if !strings.Contains(l[1], " No backup running ") || !strings.Contains(l[3], "No guest backup is running right now.") ||
		!strings.Contains(l[5], "./backup.sh <guest>") || !strings.HasPrefix(l[24], "Waiting for a backup...") {
		for _, s := range l {
			t.Log(s)
		}
		t.Error("notice")
	}
}
