package watch

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/norandom/restic-pve/internal/stream/blockmap"
	"github.com/norandom/restic-pve/retro"
)

var t0 = time.Date(2026, 11, 17, 14, 0, 0, 0, time.Local)

// words are the texts of a neutral machine.
var words = Words{Title: "restic-pve v9.9.9 · backup machine · watch", Start: "./start <guest>"}

// state is a running backup of a 16 GiB disk with a map of 1320 cells: the first 300 cells
// passed (every third one held data, all handed on but the last), the rest not read yet.
func state() *blockmap.State {
	m := bytes.Repeat([]byte{blockmap.CellUnread}, 1320)
	for i := range 300 {
		m[i] = blockmap.CellEmpty
		if i%3 == 0 {
			m[i] = blockmap.CellHanded
		}
	}
	m[298] = blockmap.CellData // read, not handed on yet
	m[300] = blockmap.CellData // being read: data arrived, not complete
	return &blockmap.State{VMID: 100, Name: "web", Mode: "full", Status: blockmap.Running, Started: t0,
		Updated: t0.Add(83 * time.Second), Disk: 16 << 30, Read: 16 << 30 * 301 / 1320, Data: 1 << 30,
		Handed: 1 << 30, Writing: true, Reading: []int{300}, Map: m}
}

func sim(t *testing.T) tcell.SimulationScreen {
	t.Helper()
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Fini)
	s.SetSize(80, 25)
	return s
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

func draw(t *testing.T, snap Snapshot, watched bool, now time.Time) []string {
	t.Helper()
	scr := sim(t)
	th := retro.NewTheme(scr, true)
	var sp Speed
	sp.Add(snap.Live)
	Build(snap, &sp, watched, now, 80, 25, words).Draw(scr, th)
	return lines(scr)
}

func want(t *testing.T, l []string, checks ...string) {
	t.Helper()
	all := strings.Join(l, "\n")
	for _, c := range checks {
		if !strings.Contains(all, c) {
			for _, s := range l {
				t.Log(s)
			}
			t.Fatalf("screen lacks %q", c)
		}
	}
}

// mapCells counts the glyphs in the map rows.
func mapCells(l []string) map[rune]int {
	n := map[rune]int{}
	for _, row := range l[1:18] {
		for _, r := range row {
			n[r]++
		}
	}
	return n
}

func TestRunning(t *testing.T) {
	s := state()
	l := draw(t, Snapshot{Live: s}, true, s.Updated)
	want(t, l, " Guest 100 (web) · full backup · 16.0 GiB disk", "q=Quit", "Block 100", "22%",
		"Elapsed Time: 00:01:23", "Full backup", "· - Not read yet", "Guest 100:  1 block = 12 MiB",
		"Writing...  ", "q leaves the view, the backup goes on")
	// the run: 100 handed cells; then W at its end, r on the cell being read
	if row2 := []rune(l[2]); !strings.HasPrefix(l[1], " "+strings.Repeat("◘", 78)) || string(row2[1:24]) != strings.Repeat("◘", 22)+"W" {
		t.Fatalf("run:\n%s\n%s", l[1], l[2])
	}
	n := mapCells(l)
	if n['r'] != 1 || n['W'] != 1 || n['·'] != 1320-301 {
		t.Errorf("map %v", n)
	}
	// reading, not writing: "Reading..." and no W
	s.Writing = false
	l = draw(t, Snapshot{Live: s}, true, s.Updated)
	want(t, l, "Reading...  ")
	if mapCells(l)['W'] != 0 {
		t.Error("W while nothing is written")
	}
}

func TestPriorMapStart(t *testing.T) {
	s := state()
	prior := state()
	for i := range prior.Map {
		prior.Map[i] = blockmap.CellEmpty
		if i%10 == 0 {
			prior.Map[i] = blockmap.CellHanded
		}
	}
	prior.Status = blockmap.Done
	// at the start nothing is read: the last layout shows as blocks with data
	for i := range s.Map {
		s.Map[i] = blockmap.CellUnread
	}
	s.Reading, s.Writing = []int{0}, false
	l := draw(t, Snapshot{Live: s, Prior: prior}, true, s.Updated)
	n := mapCells(l)
	if n['◘'] != 131 || n['r'] != 1 || n['·'] != 1320-132 {
		t.Fatalf("prior start %v", n)
	}
	// Load picks the last map up only when it fits the disk
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, blockmap.LiveFile), s.Encode(), 0o644)
	os.WriteFile(filepath.Join(dir, blockmap.LastFile(100)), prior.Encode(), 0o644)
	if got := Load(dir); got.Live == nil || got.Prior == nil {
		t.Fatal("prior not loaded")
	}
	prior.Disk++
	os.WriteFile(filepath.Join(dir, blockmap.LastFile(100)), prior.Encode(), 0o644)
	if got := Load(dir); got.Prior != nil {
		t.Fatal("a last map of another disk size was used")
	}
}

func TestFinishedAndFailed(t *testing.T) {
	s := state()
	for i := range s.Map {
		if s.Map[i] != blockmap.CellHanded {
			s.Map[i] = blockmap.CellEmpty
		}
	}
	s.Status, s.Read, s.Updated = blockmap.Done, s.Disk, t0.Add(31*time.Minute+12*time.Second)
	l := draw(t, Snapshot{Live: s}, true, s.Updated.Add(time.Hour))
	want(t, l, "Block 100", "100%", "Done in 00:31:12", "Finished. Press q to quit.", "restic now completes the snapshot")
	if n := mapCells(l); n['r'] != 0 || n['W'] != 0 || n['·'] != 0 {
		t.Errorf("end map %v", n)
	}
	// a run that ended before the viewer started: a sentence, not an old map
	l = draw(t, Snapshot{Live: s}, false, s.Updated.Add(time.Hour))
	want(t, l, "No backup is running", "Guest 100 (web),", "00:31:12", "./start <guest>")

	f := state()
	f.Status, f.Message = blockmap.Failed, "stream cut: unexpected EOF"
	l = draw(t, Snapshot{Live: f}, true, f.Updated)
	want(t, l, "Failed after 00:01:23", "Failed: stream cut: unexpected EOF", "its job's log says why")
	if mapCells(l)['r'] != 0 {
		t.Error("r after the end")
	}
}

func TestNoBackupStaleAndStarting(t *testing.T) {
	l := draw(t, Snapshot{}, false, t0)
	want(t, l, "restic-pve v9.9.9 · backup machine · watch", "No backup running",
		"No backup is running on this machine right now.", "Start one with: ./start <guest>.",
		"Waiting for a backup...")
	s := state()
	l = draw(t, Snapshot{Live: s}, true, s.Updated.Add(Stale+time.Second))
	want(t, l, "stopped updating", "14:01:23", "probably stopped; its job's log says why")
	s.Map, s.Disk = nil, 0
	l = draw(t, Snapshot{Live: s}, true, s.Updated)
	want(t, l, "waits for the Proxmox host")
	s.Status, s.Message = blockmap.Failed, "ssh: connect to host: timeout"
	l = draw(t, Snapshot{Live: s}, true, s.Updated)
	want(t, l, "failed before the disk", "ssh: connect", "its job's log says why")
	l = draw(t, Snapshot{Err: blockmap.ErrFormat}, false, t0)
	want(t, l, "cannot be read")
}

func TestLine(t *testing.T) {
	s := state()
	for _, c := range []struct {
		snap Snapshot
		now  time.Time
		want string
	}{
		{Snapshot{}, t0, "no backup is running; start one with ./start <guest>"},
		{Snapshot{Live: s}, s.Updated, "guest 100 (web): reading 22% 3.6 GiB/16.0 GiB, 45 MiB/s on average, 1.0 GiB of data in the backup stream, elapsed 00:01:23"},
		{Snapshot{Live: s}, s.Updated.Add(time.Minute), "stopped reporting at 14:01:23"},
	} {
		if got := Line(c.snap, c.now, words.Start); !strings.Contains(got, c.want) {
			t.Errorf("%q lacks %q", got, c.want)
		}
	}
	s.Status = blockmap.Done
	if got := Line(Snapshot{Live: s}, s.Updated, words.Start); !strings.Contains(got, "the last one, guest 100 (web), finished") {
		t.Error(got)
	}
}

func TestSpeed(t *testing.T) {
	var sp Speed
	s := state()
	for i := range 20 {
		c := *s
		c.Updated, c.Read = t0.Add(time.Duration(i)*500*time.Millisecond), uint64(i)*20<<20
		sp.Add(&c)
	}
	if r := sp.Rate(s); r < 39<<20 || r > 41<<20 {
		t.Errorf("rate %v", r)
	}
	c := *s
	c.Updated, c.Read = t0, 0 // a new run
	sp.Add(&c)
	if len(sp.samples) != 1 {
		t.Error("samples of the old run kept")
	}
}

// TestRunQuits: the viewer draws from the directory and quits on q; it never writes there.
func TestRunQuits(t *testing.T) {
	dir := t.TempDir()
	s := state()
	s.Updated = time.Now()
	os.WriteFile(filepath.Join(dir, blockmap.LiveFile), s.Encode(), 0o644)
	scr := sim(t)
	frames := make(chan struct{}, 100)
	done := make(chan error)
	go func() {
		done <- Run(scr, Options{Dir: dir, Color: true, Poll: 10 * time.Millisecond, Ready: func() {
			select {
			case frames <- struct{}{}:
			default:
			}
		}})
	}()
	<-frames
	<-frames
	scr.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("q did not quit")
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Errorf("the viewer wrote to the state directory: %v", ents)
	}
}

// TestRunClosesItselfWhenIdle: without a running backup the viewer ends by itself after Idle,
// so a viewer whose terminal went away cannot run for days; with a running backup it stays.
func TestRunClosesItselfWhenIdle(t *testing.T) {
	dir := t.TempDir() // no state file: no backup running
	done := make(chan error, 1)
	go func() {
		done <- Run(sim(t), Options{Dir: dir, Color: true, Poll: 5 * time.Millisecond, Idle: 40 * time.Millisecond})
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrIdle) {
			t.Fatalf("got %v, want ErrIdle", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the idle viewer did not close itself")
	}

	s := state() // a running backup with a fresh file
	s.Updated = time.Now()
	os.WriteFile(filepath.Join(dir, blockmap.LiveFile), s.Encode(), 0o644)
	scr := sim(t)
	go func() {
		done <- Run(scr, Options{Dir: dir, Color: true, Poll: 5 * time.Millisecond, Idle: 40 * time.Millisecond})
	}()
	select {
	case err := <-done:
		t.Fatalf("the viewer of a running backup ended by itself: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	scr.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestRunRedrawsOnlyOnChange: an unchanged state is drawn once, not on every poll.
func TestRunRedrawsOnlyOnChange(t *testing.T) {
	dir := t.TempDir()
	s := state()
	s.Updated = time.Now()
	os.WriteFile(filepath.Join(dir, blockmap.LiveFile), s.Encode(), 0o644)
	scr := sim(t)
	fixed := time.Now()
	polls := make(chan struct{}, 1000)
	done := make(chan error, 1)
	go func() {
		done <- Run(scr, Options{Dir: dir, Color: true, Poll: 2 * time.Millisecond, Now: func() time.Time { return fixed },
			Ready: func() { polls <- struct{}{} }})
	}()
	for i := 0; i < 20; i++ {
		<-polls
	}
	scr.SetContent(0, 0, 'Z', nil, tcell.StyleDefault) // a mark that a redraw would overwrite
	for i := 0; i < 20; i++ {
		<-polls
	}
	if r, _, _, _ := scr.GetContent(0, 0); r != 'Z' {
		t.Error("an unchanged state was drawn again")
	}
	scr.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	<-done
}

// TestWordsFromTheMachine: the title and the start command come from Words; no screen or line
// names a script of one site or the former program.
func TestWordsFromTheMachine(t *testing.T) {
	s, f, stale, early := state(), state(), state(), state()
	f.Status, f.Message = blockmap.Failed, "x"
	early.Map, early.Status, early.Message = nil, blockmap.Failed, "x"
	var all []string
	for _, c := range []struct {
		snap    Snapshot
		watched bool
		now     time.Time
	}{
		{Snapshot{}, false, t0}, {Snapshot{Live: s}, true, s.Updated}, {Snapshot{Live: f}, true, f.Updated},
		{Snapshot{Live: f}, false, f.Updated}, {Snapshot{Live: stale}, true, stale.Updated.Add(time.Hour)},
		{Snapshot{Live: early}, true, early.Updated}, {Snapshot{Err: blockmap.ErrFormat}, false, t0},
	} {
		all = append(all, draw(t, c.snap, c.watched, c.now)...)
		all = append(all, Line(c.snap, c.now, words.Start))
	}
	text := strings.Join(all, "\n")
	for _, bad := range []string{"status.sh", "backup.sh", "vmastream"} {
		if strings.Contains(text, bad) {
			t.Errorf("%q in the view:\n%s", bad, text)
		}
	}
}
