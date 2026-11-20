package status

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/norandom/restic-pve/internal/facts"
	"github.com/norandom/restic-pve/internal/pieces"
	"github.com/norandom/restic-pve/retro"
)

// readsFacts: web reads only changes (last run 1.2 GiB read, full check ok); db had a
// difference in its full check and has no run recorded since.
func readsFacts() facts.Facts {
	f := safeFacts()
	f.Config.Proxmox = []facts.GuestSource{{Name: "proxmox", Guests: []facts.Guest{{VMID: 100, Alias: "web"}, {VMID: 101, Alias: "db"}}}}
	f.Reads = map[string]facts.GuestReads{
		"web": {Last: &facts.GuestRun{Ended: *at("2026-11-27T05:04:00Z"), Kind: "changes", ReadBytes: 1288490188, SkippedBytes: 132929658470},
			Record: &pieces.RecordState{State: "based", LastCheck: &pieces.Check{Date: "2026-11-27", Result: "ok"}}},
		"db": {Record: &pieces.RecordState{State: "none", LastCheck: &pieces.Check{Date: "2026-11-26", Result: "difference"},
			Fault: &pieces.Fault{Date: "2026-11-26", Disks: []string{"drive-scsi0"}, Pieces: 3}}},
	}
	return f
}

const reads80 = ` restic-pve v9.9.9 · backup machine · Status                 checked 12 min ago
 ┌──────────────── Is there a recent copy away from the site? ────────────────┐
 │ ■ yes  files  wiki             1 day     Wed 18 Nov 05:41                  │
 │ ■ yes  guest  web              4 days    Sun 15 Nov 01:00                  │
 └────────────────────────────────────────────────────────────────────────────┘
 ┌─────────────────────── Guests that read only changes ──────────────────────┐
 │ db       no run with changed reads recorded yet                            │
 │          FAULT: full check found a difference on 2026-11-26 (3 pieces of … │
 │ web      changes: read 1.2 GiB, skipped 123.8 GiB (unchanged)              │
 │          Fri 27 Nov 05:04 · full check: ok 2026-11-27                      │
 └────────────────────────────────────────────────────────────────────────────┘
 ┌──────────────────────── Have I seen a restore work? ───────────────────────┐
 │ Automatic restore test  Sun 15 Nov  passed (web)                           │
 │ Drill (you checked it)  none recorded. Do one: docs/guide/07-restore.md    │
 │ On the bad day          docs/guide/08-when-something-goes-wrong.md         │
 └────────────────────────────────────────────────────────────────────────────┘
 ┌─────────────────────────────── Now and next ───────────────────────────────┐
 │ Running   nothing now                                                      │
 │ Next      local sources Sat 21 Nov 05:00 · guests Sun 22 Nov 00:00         │
 │ Space     819 GiB free of 2 TiB · no warning                               │
 │ Updates   v9.9.9 · updated with this machine's image            Alerts  on │
 │ Password  copy proven Tue 17 Nov 2026                                      │
 └────────────────────────────────────────────────────────────────────────────┘
 All copies are recent and checked.
 Enter details  k Keys  s Space  w Watch  r Refresh  ? Help  q Quit`

func TestReadsBox(t *testing.T) {
	old := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = old })
	s := sim(t, 80, 25)
	th := retro.NewTheme(s, true)
	f := readsFacts()
	f.CheckedAt = now.Add(-12 * time.Minute)
	sc := &Screen{Title: "restic-pve v9.9.9 · backup machine · Status", Model: Build(f, "./backup.sh <source>", now), Zone: "UTC"}
	sc.Draw(s, th)
	if got := strings.Join(text(s), "\n"); got != reads80 {
		t.Errorf("layout:\n%s", got)
	}
	// the fault is drawn in the error colours, the other lines are not
	if _, _, st, _ := s.GetContent(13, 7); st != th.Error {
		t.Errorf("fault line style %v", st)
	}
	if _, _, st, _ := s.GetContent(13, 9); st != th.Base {
		t.Errorf("check line style %v", st)
	}
	// at 80 x 24 the copies keep their rows: one guest shows (the one with the fault)
	s.SetSize(80, 24)
	sc.Draw(s, th)
	if l := text(s); l[3] != " │ ■ yes  guest  web              4 days    Sun 15 Nov 01:00                  │" ||
		!strings.Contains(l[5], " Guests that read only changes (1 of 2: Enter) ") || !strings.HasPrefix(l[6], " │ db ") {
		t.Errorf("80x24:\n%s", strings.Join(l, "\n"))
	}
	s.SetSize(80, 25)
	// Enter: every guest's lines in full, with the command that clears the fault
	sc.Key(tcell.NewEventKey(tcell.KeyEnter, 0, 0))
	sc.Draw(s, th)
	all := strings.Join(text(s), "\n")
	for _, w := range []string{"web      changes: read 1.2 GiB, skipped 123.8 GiB (unchanged)", "full check: ok 2026-11-27",
		"./backup.sh proxmox --clear-fault db"} {
		if !strings.Contains(all, w) {
			t.Errorf("details lack %q:\n%s", w, all)
		}
	}
	// without room (60 x 20) the box gives way to the copies; Enter still lists it
	s.SetSize(60, 20)
	sc.Details = false
	sc.Draw(s, th)
	if all := strings.Join(text(s), "\n"); strings.Contains(all, "Guests that read only changes") {
		t.Errorf("60x20:\n%s", all)
	}
}

func TestReadsPlain(t *testing.T) {
	m := Build(readsFacts(), "./backup.sh <source>", now)
	got := strings.Join(m.Plain(), "\n")
	for _, w := range []string{
		"reads  web  changes  ended 2026-11-27T05:04Z  read 1.2GiB  skipped 123.8GiB  full-check ok 2026-11-27",
		"reads  db  none  full-check difference 2026-11-26",
		"fault  db  2026-11-26  3 pieces  drive-scsi0  do: ./backup.sh proxmox --clear-fault db",
	} {
		if !strings.Contains(got, w+"\n") {
			t.Errorf("plain lacks %q:\n%s", w, got)
		}
	}
	// the lines of each kind of run, and before the first full check
	for _, c := range []struct {
		run  facts.GuestRun
		want string
	}{
		{facts.GuestRun{Kind: "full", ReadBytes: 134217728000}, "full read: read 125.0 GiB"},
		{facts.GuestRun{Kind: "check", ReadBytes: 134217728000}, "full check: read 125.0 GiB"},
		{facts.GuestRun{Kind: "changes", ReadBytes: 0, SkippedBytes: 134217728000}, "changes: read 0 B, skipped 125.0 GiB (unchanged)"},
	} {
		r := readsRow("proxmox", "web", facts.GuestReads{Last: &c.run}, "./backup.sh <source>")
		if r.Run != c.want || r.Check != "full check: none yet" {
			t.Errorf("%s: %q %q", c.run.Kind, r.Run, r.Check)
		}
	}
}
