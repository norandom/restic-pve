// Package demo shows the block map of the backup view with a simulated run, to judge the
// retro look. It reads no data: the disk, its blocks and the run are made up.
package demo

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/norandom/restic-pve/internal/retro"
	"github.com/rivo/tview"
	"golang.org/x/term"
)

// Mode is the kind of simulated run.
type Mode int

const (
	Full        Mode = iota // every block with data is read and goes into the backup
	Incremental             // unchanged blocks are passed quickly; only new blocks are written
)

func (m Mode) String() string {
	if m == Incremental {
		return "Incremental backup"
	}
	return "Full backup"
}

// DiskGiB is the size of the simulated disk.
const DiskGiB = 125

// Tick is the time of one step of the simulation.
const Tick = 25 * time.Millisecond

// Sim is a simulated run over a disk of blocks. Step moves it on by one tick.
type Sim struct {
	Mode     Mode
	Cells    []retro.State
	data     []bool // the block holds data
	changed  []bool // the block changed since the last backup (incremental)
	pos      int    // the block being worked on
	wait     int    // ticks left in the current state
	BlockMiB int
}

// NewSim lays out a disk of DiskGiB over at most cells blocks. The layout is always the
// same: dense at the start like a system disk, sparser towards the end.
func NewSim(mode Mode, cells int) *Sim {
	cells = max(cells, 1)
	total := DiskGiB * 1024
	blockMiB := (total + cells - 1) / cells
	n := (total + blockMiB - 1) / blockMiB
	s := &Sim{Mode: mode, Cells: make([]retro.State, n), data: make([]bool, n), changed: make([]bool, n), BlockMiB: blockMiB}
	rng := rand.New(rand.NewPCG(1994, 6))
	in, ch := false, false
	for i := range n {
		depth := float64(i) / float64(n)
		switch {
		case i < n/40:
			in = true
		case in:
			in = rng.Float64() < 0.55
		default:
			in = rng.Float64() < 0.22*(1-depth)+0.02
		}
		s.data[i] = in
		if in {
			ch = rng.Float64() < 0.04 || ch && rng.Float64() < 0.7
		} else {
			ch = false
		}
		s.changed[i] = ch
		if in {
			s.Cells[i] = retro.Data
		}
	}
	return s
}

// Pos is the block being worked on; Len when the run is finished.
func (s *Sim) Pos() int { return s.pos }

// Percent is the share of blocks passed.
func (s *Sim) Percent() int { return s.pos * 100 / len(s.Cells) }

// Finished reports whether every block was passed.
func (s *Sim) Finished() bool { return s.pos >= len(s.Cells) }

// isNew reports whether block i is written to the backup in this run.
func (s *Sim) isNew(i int) bool {
	if s.Mode == Incremental {
		return s.changed[i]
	}
	return s.data[i]
}

func (s *Sim) finish() {
	if s.data[s.pos] {
		s.Cells[s.pos] = retro.Done
	} else {
		s.Cells[s.pos] = retro.DoneEmpty
	}
	s.pos++
}

// Step moves the run on by one tick: the block under the marker is read ("r"), new data is
// written ("W"), and the block turns yellow behind the marker. An incremental run passes
// up to eight unchanged blocks per tick and stays longer on new ones.
func (s *Sim) Step() {
	if s.Finished() {
		return
	}
	if s.wait > 0 {
		s.wait--
		return
	}
	switch s.Cells[s.pos] {
	case retro.Reading:
		if s.isNew(s.pos) {
			s.Cells[s.pos] = retro.Writing
			s.wait = 1
			if s.Mode == Incremental {
				s.wait = 4
			}
			return
		}
		s.finish()
	case retro.Writing:
		s.finish()
	}
	if s.Mode == Incremental {
		for skip := 0; skip < 7 && !s.Finished() && !s.isNew(s.pos); skip++ {
			s.finish()
		}
	}
	if s.Finished() {
		return
	}
	s.Cells[s.pos] = retro.Reading
	switch {
	case s.Mode == Incremental && s.isNew(s.pos):
		s.wait = 2
	case s.Mode == Full && s.data[s.pos]:
		s.wait = 1
	}
}

// View is the block map screen of a simulated run.
type View struct {
	*tview.Box
	T       retro.Theme
	Sim     *Sim
	Elapsed time.Duration
}

// Legend is what the map's cells mean in the backup view.
var Legend = []retro.LegendEntry{
	{State: retro.Data, Text: "Has data"}, {State: retro.Empty, Text: "Empty"},
	{State: retro.Reading, Text: "Reading"}, {State: retro.Writing, Text: "Writing"},
	{State: retro.Done, Text: "In backup"},
}

// MapSize is the number of cells of the map on a screen of cols x rows.
func MapSize(cols, rows int) int {
	l := retro.NewLayout(cols, rows)
	return max(cols-2, 1) * max(l.BodyH, 1)
}

func (v *View) Draw(screen tcell.Screen) {
	cols, rows := screen.Size()
	t := v.T
	l := retro.NewLayout(cols, rows)
	t.Background(screen)
	t.TitleBar(screen, l.TitleBarY, cols, fmt.Sprintf("Demo · simulated %s · %d GiB disk · reads no data", lower(v.Sim.Mode), DiskGiB), "q=Quit")

	m := retro.NewBlockMap(t, 0)
	m.Cells = v.Sim.Cells
	m.SetRect(1, l.BodyY, cols-2, l.BodyH)
	m.Draw(screen)

	st := retro.NewStatusBox(t)
	st.Block, st.Percent, st.Elapsed, st.Mode = min(v.Sim.Pos()+1, len(v.Sim.Cells)), v.Sim.Percent(), v.Elapsed, v.Sim.Mode.String()
	st.SetRect(1, l.LowerY, l.LeftW, retro.LowerHeight)
	st.Draw(screen)

	lg := retro.NewLegendBox(t, Legend, fmt.Sprintf("Demo disk:   1 block = %d MiB", v.Sim.BlockMiB))
	lg.SetRect(l.RightX, l.LowerY, l.RightW(), retro.LowerHeight)
	lg.Draw(screen)

	msg := "Reading..."
	switch {
	case v.Sim.Finished():
		msg = "Finished. Press q to quit."
	case v.Sim.Cells[v.Sim.Pos()] == retro.Writing:
		msg = "Writing..."
	}
	t.BottomBar(screen, l.BottomBarY, cols, msg, t.BarMsg, "restic-pve demo")
	screen.HideCursor()
}

func lower(m Mode) string {
	if m == Incremental {
		return "incremental backup"
	}
	return "full backup"
}

// Run shows the simulated run on an initialised screen until q or Ctrl-C.
func Run(screen tcell.Screen, mode Mode, color bool) error {
	t := retro.NewTheme(screen, color)
	t.Apply()
	cols, rows := screen.Size()
	v := &View{Box: tview.NewBox(), T: t, Sim: NewSim(mode, MapSize(cols, rows))}
	app := tview.NewApplication().SetScreen(retro.Ready(screen)).SetRoot(v, true)
	app.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyCtrlC || ev.Key() == tcell.KeyRune && (ev.Rune() == 'q' || ev.Rune() == 'Q') {
			app.Stop()
		}
		return nil
	})
	start := time.Now()
	quit := make(chan struct{})
	go func() {
		tick := time.NewTicker(Tick)
		defer tick.Stop()
		for {
			select {
			case <-quit:
				return
			case <-tick.C:
				app.QueueUpdateDraw(func() {
					if !v.Sim.Finished() {
						v.Sim.Step()
						v.Elapsed = time.Since(start)
					}
				})
			}
		}
	}()
	err := app.Run()
	close(quit)
	return err
}

// Main is "restic-pve demo [full|incremental]": it needs a terminal.
func Main(args []string, getenv func(string) string) error {
	mode := Full
	if len(args) == 1 && args[0] == "incremental" {
		mode = Incremental
	}
	if !term.IsTerminal(0) || !term.IsTerminal(1) {
		return errors.New("the demo needs a terminal")
	}
	screen, err := tcell.NewScreen()
	if err != nil {
		return err
	}
	if err := screen.Init(); err != nil {
		return err
	}
	defer screen.Fini()
	return Run(screen, mode, getenv("NO_COLOR") == "")
}
