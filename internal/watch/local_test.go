package watch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/norandom/restic-pve/retro"
	"golang.org/x/sys/unix"
)

var l0 = time.Date(2026, 11, 19, 5, 0, 0, 0, time.UTC)

// statusRunning is status.json in the middle of the second unit of a local run.
const statusRunning = `{"run_id": "r9", "started": "2026-11-19T05:00:00.000000Z", "updated": "%s",
 "current_unit": "sites/blog", "step": "backup", "percent": 35.0, "bytes_done": %d, "total_bytes": 12884901888,
 "eta_s": 600, "units": {"wiki": "success", "sites/blog": "running", "sites/shop": "pending"},
 "data_added": {"wiki": 125829120}, "run_status": "running", "io_paused": false, "last_message": null}`

const statusDone = `{"run_id": "r9", "started": "2026-11-19T05:00:00.000000Z", "updated": "2026-11-19T05:41:00.000000Z",
 "current_unit": null, "units": {"wiki": "success", "sites/blog": "success", "sites/shop": "partial"},
 "data_added": {"sites/shop": 1024, "wiki": 125829120, "sites/blog": 1181116006}, "run_status": "success"}`

func writeStatus(t *testing.T, dir, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "status.json"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// holdRunLock holds <dir>/run.lock as a running backupctl does.
func holdRunLock(t *testing.T, dir string) {
	t.Helper()
	f, err := os.Create(filepath.Join(dir, "run.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
}

func localSample(t *testing.T, dir string, at time.Time, done int64) Local {
	t.Helper()
	writeStatus(t, dir, fmt.Sprintf(statusRunning, at.UTC().Format("2006-01-02T15:04:05.000000Z"), done))
	return LoadLocal(dir)
}

func TestLocalRunning(t *testing.T) {
	dir := t.TempDir()
	holdRunLock(t, dir)
	var sp LocalSpeed
	lc := localSample(t, dir, l0.Add(12*time.Minute), 4<<30)
	if !lc.Running || lc.S.CurrentUnit != "sites/blog" {
		t.Fatalf("%+v", lc)
	}
	sp.Add(lc.S)
	now := l0.Add(12*time.Minute + 31*time.Second)
	if got := LocalLine(lc, &sp, now); got != "local backup: sites/blog reading 35% 4.0 GiB/12.0 GiB, speed not measured yet, elapsed 00:12:31, new data shown when this source ends" {
		t.Errorf("line %q", got)
	}
	// 30 s later 930 MiB more: 31 MiB/s
	lc = localSample(t, dir, l0.Add(12*time.Minute+30*time.Second), 4<<30+930<<20)
	sp.Add(lc.S)
	if r, ok := sp.Rate(); !ok || int(r) != 31<<20 {
		t.Errorf("rate %v %v", r, ok)
	}
	scr := sim(t)
	s80x24(scr)
	th := retro.NewTheme(scr, true)
	BuildLocal(lc, &sp, now, words).Draw(scr, th)
	l := lines(scr)
	want(t, l, "restic-pve v9.9.9 · backup machine · watch", "q=Leave view", " Backing up sites ",
		"Source    sites (unit sites/blog)", "Read      4.9 GiB of 12.0 GiB (35%)", "Elapsed   00:12:31",
		"Speed     31 MiB/s over the last 30 s", "New data  shown when this source ends", "Finished  new data wiki +120 MiB",
		"Reading...", "q leaves the view, the backup goes on")
	if !strings.Contains(strings.Join(l, "\n"), "█") {
		t.Error("no progress track")
	}
	// a pause for busy disks is said in words
	lc.S.IOPaused = true
	BuildLocal(lc, &sp, now, words).Draw(scr, th)
	want(t, lines(scr), "Paused    the machine's disks are busy; the backup waits and goes on by", "itself", "Paused...")
}

func s80x24(s tcell.SimulationScreen) { s.SetSize(80, 24) }

func TestLocalFinishedAndNone(t *testing.T) {
	old := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = old })
	dir := t.TempDir()
	// no run at all: the start hint
	if got := LineAll(Snapshot{}, LoadLocal(dir), &LocalSpeed{}, l0, "./start <source>"); got != "no backup is running; start one with ./start <source>" {
		t.Errorf("none: %q", got)
	}
	// a finished run: its new data per source, in the order of the run
	writeStatus(t, dir, statusDone)
	lc := LoadLocal(dir)
	if lc.Running {
		t.Fatal("a finished run runs")
	}
	got := LineAll(Snapshot{}, lc, &LocalSpeed{}, l0.Add(time.Hour), "./start <source>")
	if got != "no backup is running; start one with ./start <source>; the last local run ended at 2026-11-19 05:41 (success), new data wiki +120 MiB · sites/blog +1.1 GiB · sites/shop +1 KiB" {
		t.Errorf("finished: %q", got)
	}
	// the file says running but nothing holds the lock: stopped without a result
	writeStatus(t, dir, fmt.Sprintf(statusRunning, "2026-11-19T05:12:00.000000Z", 5))
	if got := LineAll(Snapshot{}, LoadLocal(dir), &LocalSpeed{}, l0.Add(time.Hour), "x"); !strings.HasSuffix(got,
		"the last local run stopped at 2026-11-19 05:12 without a result (its job's log says why)") {
		t.Errorf("stopped: %q", got)
	}
}

// TestRunShowsLocal: the viewer shows a running local backup, then, when it has ended, the
// notice with its result and the start hint; it never writes.
func TestRunShowsLocal(t *testing.T) {
	state := t.TempDir()
	view := filepath.Join(state, "guests", "view")
	holdRunLock(t, state)
	localSample(t, state, time.Now(), 1<<30)
	scr := sim(t)
	frames := make(chan string, 1000)
	done := make(chan error, 1)
	go func() {
		done <- Run(scr, Options{Dir: view, State: state, Words: words, Color: true, Poll: 5 * time.Millisecond,
			Ready: func() { frames <- strings.Join(lines(scr), "\n") }})
	}()
	waitFrame := func(want string) {
		t.Helper()
		for {
			select {
			case f := <-frames:
				if strings.Contains(f, want) {
					return
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("no frame with %q", want)
			}
		}
	}
	waitFrame("Backing up sites")
	before, _ := os.ReadDir(state)
	writeStatus(t, state, statusDone)
	waitFrame("The last local run ended at")
	waitFrame("Start one with: ./start <guest>.")
	scr.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadDir(state); len(after) != len(before) {
		t.Errorf("the viewer wrote: %v", after)
	}
}
