package watch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/norandom/restic-pve/internal/facts"
	"github.com/norandom/restic-pve/internal/stream/blockmap"
	"golang.org/x/sys/unix"
)

// jobState is a state root with a job record; lock holds the job's lock as backupctl does.
func jobState(t *testing.T, name string, started time.Time, lock bool) string {
	t.Helper()
	st := t.TempDir()
	jobs := filepath.Join(st, facts.JobsDir)
	os.MkdirAll(jobs, 0o755)
	os.MkdirAll(filepath.Join(st, facts.ViewDir), 0o755)
	os.WriteFile(filepath.Join(jobs, name+".json"), []byte(`{"schema": 1, "name": "`+name+`", "started": "`+
		started.UTC().Format(time.RFC3339)+`", "ended": null, "exit": null, "run_id": null}`), 0o644)
	if lock {
		f, err := os.Create(filepath.Join(jobs, name+".lock"))
		if err != nil {
			t.Fatal(err)
		}
		if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
	}
	return st
}

func plain(st string, now time.Time) string {
	var sp LocalSpeed
	lc := LoadLocal(st)
	sp.Add(lc.S)
	return JobsLine(Jobs(st), Load(filepath.Join(st, facts.ViewDir)), lc, &sp, now, words.Start)
}

// TestStartingGuests: starting (lock held, no live view of this run) → reading (the live file
// of this run) → finished; and getting ready for too long.
func TestStartingGuests(t *testing.T) {
	start := t0.Truncate(time.Second)
	st := jobState(t, "guests", start, true)
	view := filepath.Join(st, facts.ViewDir)
	// an old live file of the last run does not count
	old := state()
	old.Started, old.Updated, old.Status = start.Add(-24*time.Hour), start.Add(-23*time.Hour), blockmap.Done
	os.WriteFile(filepath.Join(view, blockmap.LiveFile), old.Encode(), 0o644)
	now := start.Add(42 * time.Second)
	if l := plain(st, now); l != "starting: guests job began 42s ago; reading has not begun yet" {
		t.Errorf("starting: %q", l)
	}
	f := BuildStarting(Jobs(st)[0], now, words)
	all := strings.Join(f.Notice.Lines, " ")
	if f.Notice.Heading != "Getting ready" || !strings.Contains(all, "The backup of guests started 40 seconds ago and is getting ready.") ||
		!strings.Contains(all, "checks the guest on the Proxmox host") || !strings.Contains(all, "Nothing to do") {
		t.Errorf("notice %+v", f.Notice)
	}
	// too long
	late := start.Add(12*time.Minute + 5*time.Second)
	if l := plain(st, late); !strings.Contains(l, "began 12m ago; reading has not begun yet, longer than usual; look at its result: ./status.sh") {
		t.Errorf("too long: %q", l)
	}
	f = BuildStarting(Jobs(st)[0], late, words)
	if all := strings.Join(f.Notice.Lines, " "); !strings.Contains(all, "has been getting ready for 12 minutes. That is longer than usual.") ||
		!strings.Contains(all, "Look at its result: ./status.sh.") {
		t.Errorf("too long notice %+v", f.Notice)
	}
	// reading begins: the live file of this run
	s := state()
	s.Started, s.Updated = start.Add(70*time.Second), start.Add(80*time.Second)
	os.WriteFile(filepath.Join(view, blockmap.LiveFile), s.Encode(), 0o644)
	if l := plain(st, start.Add(81*time.Second)); !strings.HasPrefix(l, "guest 100 (web): reading") {
		t.Errorf("reading: %q", l)
	}
	// the stream is done, the job still holds its lock: never "no backup is running"
	s.Status = blockmap.Done
	os.WriteFile(filepath.Join(view, blockmap.LiveFile), s.Encode(), 0o644)
	if l := plain(st, start.Add(90*time.Second)); !strings.HasPrefix(l, "running: guests job began 1m ago; between two steps; the last one, guest 100 (web), finished") {
		t.Errorf("between steps: %q", l)
	}
}

// TestJobWithoutLock: a record without an end whose lock is free ended without a result; it
// is not "starting", and the line says that no backup runs.
func TestJobWithoutLock(t *testing.T) {
	st := jobState(t, "guests", t0, false)
	if j := Jobs(st); len(j) != 0 {
		t.Errorf("jobs %+v", j)
	}
	if l := plain(st, t0.Add(time.Minute)); !strings.HasPrefix(l, "no backup is running") {
		t.Errorf("line %q", l)
	}
	// a restore job is not a backup that gets ready
	st = jobState(t, "restore-test", t0, true)
	if j := Jobs(st); len(j) != 0 {
		t.Errorf("restore counted: %+v", j)
	}
}

// TestStartingLocal: a local job is starting until status.json has a running entry of this run.
func TestStartingLocal(t *testing.T) {
	start := t0.Truncate(time.Second)
	st := jobState(t, "wiki", start, true)
	if l := plain(st, start.Add(20*time.Second)); l != "starting: wiki job began 20s ago; reading has not begun yet" {
		t.Errorf("local starting: %q", l)
	}
	f := BuildStarting(Jobs(st)[0], start.Add(20*time.Second), words)
	if all := strings.Join(f.Notice.Lines, " "); !strings.Contains(all, "The backup of wiki started 20 seconds ago") ||
		!strings.Contains(all, "the progress appears when reading begins") {
		t.Errorf("local notice %+v", f.Notice)
	}
	os.WriteFile(filepath.Join(st, facts.StatusFile), []byte(`{"run_id": "r2", "started": "`+start.Add(10*time.Second).UTC().Format(time.RFC3339)+
		`", "updated": "`+start.Add(30*time.Second).UTC().Format(time.RFC3339)+`", "run_status": "running", "current_unit": "wiki", "bytes_done": 1024}`), 0o644)
	if j := Jobs(st); len(j) != 1 || j[0].Starting {
		t.Errorf("reading began: %+v", j)
	}
}

// TestRunStarting: the screen shows the notice, counts in steps of five seconds, does not
// close itself while the job gets ready, and switches to the map by itself.
func TestRunStarting(t *testing.T) {
	start := time.Now().Truncate(time.Second)
	st := jobState(t, "guests", start, true)
	view := filepath.Join(st, facts.ViewDir)
	scr := sim(t)
	frames := make(chan string, 1024)
	done := make(chan error)
	go func() {
		done <- Run(scr, Options{Dir: view, State: st, Words: words, Color: true, Poll: 20 * time.Millisecond,
			Idle: 100 * time.Millisecond, Ready: func() {
				select {
				case frames <- strings.Join(lines(scr), "\n"):
				default:
				}
			}})
	}()
	wait := func(want string) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case f := <-frames:
				if strings.Contains(f, want) {
					return
				}
			case err := <-done:
				t.Fatalf("the view ended (%v) before %q", err, want)
			case <-deadline:
				t.Fatalf("no frame with %q", want)
			}
		}
	}
	wait("is getting ready")
	time.Sleep(300 * time.Millisecond) // longer than Idle: it must stay open
	s := state()
	s.Started, s.Updated = time.Now(), time.Now()
	os.WriteFile(filepath.Join(view, blockmap.LiveFile), s.Encode(), 0o644)
	wait("Guest 100 (web) · full backup")
	scr.InjectKey(tcell.KeyRune, 'q', 0)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
