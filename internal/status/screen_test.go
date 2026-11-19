package status

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/norandom/restic-pve/internal/facts"
	"github.com/norandom/restic-pve/internal/tui"
	"github.com/norandom/restic-pve/retro"
)

func sim(t *testing.T, cols, rows int) tcell.SimulationScreen {
	t.Helper()
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Fini)
	s.SetSize(cols, rows)
	return s
}

func text(s tcell.SimulationScreen) []string {
	s.Show()
	cells, w, h := s.GetContents()
	out := make([]string, h)
	for y := range h {
		var b strings.Builder
		for x := range w {
			b.WriteRune(cells[y*w+x].Runes[0])
		}
		out[y] = strings.TrimRight(b.String(), " ")
	}
	return out
}

// layout80 is the status screen at 80 x 24 for the facts of TestNotSafeAndUnknown, with the
// machine's zone UTC.
const layout80 = ` restic-pve v9.9.9 · backup machine · Status                 checked 12 min ago
 ┌──────────────── Is there a recent copy away from the site? ────────────────┐
 │ ■ NO   files  sites/blog       5 days    check failed: ./backup.sh sites … │
 │ ■ NO   guest  db               -         no copy: ./backup.sh db           │
 │ ■ NO   files  old              18 days   too old: ./backup.sh old          │
 │ ■ yes  files  wiki             1 day     Wed 18 Nov 05:41                  │
 │ ■ yes  guest  web              4 days    Sun 15 Nov 01:00                  │
 └────────────────────────────────────────────────────────────────────────────┘
 ┌──────────────────────── Have I seen a restore work? ───────────────────────┐
 │ Automatic restore test  Sun 15 Nov  FAILED (web)                           │
 │ Drill (you checked it)  none recorded. Do one: docs/guide/07-restore.md    │
 │ On the bad day          docs/guide/08-when-something-goes-wrong.md         │
 └────────────────────────────────────────────────────────────────────────────┘
 ┌─────────────────────────────── Now and next ───────────────────────────────┐
 │ Running   guests since Thu 09:00                                w to watch │
 │ Next      local sources Sat 21 Nov 05:00 · guests not known                │
 │ Space     the storage does not report its free space                       │
 │ Updates   v9.9.9 · updated with this machine's image            Alerts  on │
 │ Password  copy not proven: press k (Keys) and prove it                     │
 └────────────────────────────────────────────────────────────────────────────┘
 Not safe: sites/blog failed its last integrity check. Do: ./backup.sh sites
 and read the alert

 Enter details  k Keys  s Space  w Watch  r Refresh  ? Help  q Quit`

func notSafeModel() Model {
	f := safeFacts()
	f.Instances = append(f.Instances,
		facts.Instance{Source: "sites", Name: "sites/blog", Kind: "files", Newest: at("2026-11-14T05:20:00Z"),
			Check: &facts.Check{TS: *at("2026-11-14T05:52:00Z"), OK: false}, Interval: week, Known: true},
		facts.Instance{Source: "proxmox", Name: "db", Kind: "guest", Interval: week, Known: true},
		facts.Instance{Source: "old", Name: "old", Kind: "files", Newest: at("2026-11-01T05:00:00Z"), Interval: week, Known: true},
	)
	f.RestoreTest.Status = "failed"
	f.Running = []facts.Job{{Name: "guests", Started: *at("2026-11-19T09:00:00Z"), Running: true}}
	f.Next[1].At = time.Time{}
	f.Space.About = nil
	f.Proof = nil
	f.CheckedAt = now.Add(-12 * time.Minute)
	return Build(f, "./backup.sh <source>", now)
}

func TestLayout80x24(t *testing.T) {
	old := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = old })
	s := sim(t, 80, 24)
	th := retro.NewTheme(s, true)
	sc := &Screen{Title: "restic-pve v9.9.9 · backup machine · Status", Model: notSafeModel(), Zone: "UTC"}
	sc.Draw(s, th)
	got := strings.Join(text(s), "\n")
	if got != layout80 {
		t.Errorf("layout:\n%s", got)
	}
	// safe: the verdict line says so, every row fits
	sc.Model = Build(safeFacts(), "./backup.sh <source>", now)
	sc.Draw(s, th)
	l := text(s)
	if l[17] != " All copies are recent and checked." || !strings.HasPrefix(l[2], " │ ■ yes  files  wiki             1 day     Wed 18 Nov 05:41") {
		t.Errorf("safe:\n%s", strings.Join(l, "\n"))
	}
	// Enter lists every copy; ? shows the help with the bad-day guide
	sc.Model = notSafeModel()
	sc.Key(tcell.NewEventKey(tcell.KeyEnter, 0, 0))
	sc.Draw(s, th)
	if all := strings.Join(text(s), "\n"); !strings.Contains(all, "All copies (5)") || !strings.Contains(all, "yes  files  wiki") {
		t.Errorf("details:\n%s", all)
	}
	sc.Key(tcell.NewEventKey(tcell.KeyEnter, 0, 0))
	sc.Key(tcell.NewEventKey(tcell.KeyRune, '?', 0))
	sc.Draw(s, th)
	if all := strings.Join(text(s), "\n"); !strings.Contains(all, "08-when-something-goes-wrong.md") || !strings.Contains(all, "(UTC)") {
		t.Errorf("help:\n%s", all)
	}
	// at the smallest size the box shows fewer rows and how to see the rest
	s.SetSize(60, 20)
	sc.Details = false
	sc.Help = false
	sc.Draw(s, th)
	if l := text(s); !strings.Contains(l[4], "… 3 more: press Enter") || !strings.HasPrefix(l[18], " Not safe: sites/blog") {
		t.Errorf("60x20:\n%s", strings.Join(l, "\n"))
	}
	// below 60 x 20: the plain lines
	s.SetSize(59, 24)
	sc.Help = false
	sc.Draw(s, th)
	if l := text(s); l[0] != "copy  sites/blog  no  5d  check failed  do: ./backup.sh sit" || l[1] != "es and read the alert" {
		t.Errorf("small: %q", l[:2])
	}
}

// TestNoColor: NO_COLOR keeps the layout and draws without colours.
func TestNoColor(t *testing.T) {
	s := sim(t, 80, 24)
	sc := &Screen{Title: "t", Model: notSafeModel()}
	sc.Draw(s, retro.NewTheme(s, false))
	cells, _, _ := s.GetContents()
	for _, c := range cells {
		fg, bg, _ := c.Style.Decompose()
		if fg != tcell.ColorDefault || bg != tcell.ColorDefault {
			t.Fatalf("a coloured cell %q", c.Runes)
		}
	}
	if l := text(s); !strings.Contains(l[1], "Is there a recent copy") {
		t.Errorf("layout lost: %q", l[1])
	}
}

// TestRun: the screen reads the storage once in the background, redraws only on a change,
// leaves with q or with the key of another screen, and closes itself when idle.
func TestRun(t *testing.T) {
	s := sim(t, 80, 24)
	var mu sync.Mutex
	var reads []string
	frames := make(chan string, 64)
	clock := now
	o := Options{Screen: s, Title: "restic-pve v9.9.9 · backup machine · Status", Zone: "UTC", Poll: 5 * time.Millisecond,
		Now: func() time.Time { mu.Lock(); defer mu.Unlock(); return clock },
		Read: func(refresh, cacheOnly bool) Model {
			mu.Lock()
			reads = append(reads, map[bool]string{true: "refresh", false: "open"}[refresh]+map[bool]string{true: "+cache", false: ""}[cacheOnly])
			mu.Unlock()
			return notSafeModel()
		},
		Ready: func() { frames <- strings.Join(text(s), "\n") },
	}
	drawn := 0
	wait := func(want string) {
		t.Helper()
		for {
			select {
			case f := <-frames:
				drawn++
				if strings.Contains(f, want) {
					return
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("no frame with %q", want)
			}
		}
	}
	for _, c := range []struct {
		key  rune
		next string
	}{{'k', "keys"}, {'s', "space"}, {'w', "watch"}, {'q', ""}} {
		done := make(chan string)
		go func() {
			next, err := Run(o, Build(facts.Facts{Version: "v9.9.9"}, "", now))
			if err != nil {
				t.Error(err)
			}
			done <- next
		}()
		wait("checked 12 min ago") // the background read arrived
		s.InjectKey(tcell.KeyRune, c.key, 0)
		if got := <-done; got != c.next {
			t.Errorf("%c: next %q", c.key, got)
		}
	}
	if strings.Join(reads, ",") != "open,open,open,open" {
		t.Errorf("reads %v", reads)
	}
	// idle: no key for the idle time closes the screen
	o.Idle = 50 * time.Millisecond
	o.Now = time.Now
	o.Ready = nil
	if _, err := Run(o, notSafeModel()); !errors.Is(err, tui.ErrIdle) {
		t.Errorf("idle: %v", err)
	}
}
