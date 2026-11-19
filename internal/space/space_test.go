package space

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/norandom/restic-pve/internal/facts"
	"github.com/norandom/restic-pve/retro"
)

const GiB = int64(1) << 30

func i64(v int64) *int64 { return &v }

// spaceFacts: a 2 TiB storage, runs that added up to 31 GiB, two local sources and one guest host.
func spaceFacts(free, total *int64) facts.Facts {
	ts := time.Date(2026, 11, 14, 5, 55, 0, 0, time.UTC)
	f := facts.Facts{Config: facts.Config{Base: "rclone:drive:backups", Files: []string{"wiki", "sites"},
		Retention: facts.Retention{Weekly: 8, Monthly: 6, Yearly: 2},
		Proxmox:   []facts.GuestSource{{Name: "proxmox", Guests: []facts.Guest{{VMID: 100, Alias: "web", KeepLast: 2}, {VMID: 101, Alias: "db", KeepLast: 5}}}}}}
	f.Space = facts.SpaceFacts{Remote: "drive", About: &facts.About{Free: free, Total: total, Used: i64(1200 * GiB)},
		Runs:      []facts.Run{{DataAdded: 3 * GiB}, {DataAdded: 31 * GiB}, {DataAdded: 1 * GiB}},
		RepoSizes: map[string]facts.RepoSize{"wiki": {TS: ts, Bytes: 12 * GiB}, "proxmox": {TS: ts, Bytes: 120 * GiB}},
		GuestSize: map[string]*int64{"web": i64(32 * GiB)}}
	return f
}

func check(f facts.Facts) (string, int) { return Build(f).CheckLine() }

func TestRule(t *testing.T) {
	total := i64(2048 * GiB)
	for _, c := range []struct {
		name  string
		free  *int64
		total *int64
		code  int
		line  string
	}{
		{"plenty", i64(819 * GiB), total, 0, "space ok: 819 GiB free of 2 TiB, the largest recent run added 31 GiB"},
		{"twice the largest run exactly", i64(62 * GiB), nil, 0, "space ok: 62 GiB free, the largest recent run added 31 GiB"},
		{"just below twice", i64(62*GiB - 1), nil, 1, "space low: 62 GiB free, the largest recent run added 31 GiB; lower the keep rules or add storage, see docs/guide/05-google-drive-space.md"},
		{"a tenth exactly", i64(2048 * GiB / 10), total, 0, "space ok: 205 GiB free of 2 TiB, the largest recent run added 31 GiB"},
		{"just below a tenth", i64(2048*GiB/10 - 1), total, 1, "space low: 205 GiB free of 2 TiB, less than a tenth; lower the keep rules or add storage, see docs/guide/05-google-drive-space.md"},
		{"no free reported", nil, total, 2, "space not known: the storage does not report its free space; no warning can be computed"},
	} {
		line, code := check(spaceFacts(c.free, c.total))
		if line != c.line || code != c.code {
			t.Errorf("%s: %d %q", c.name, code, line)
		}
	}
	// no run recorded: the tenth rule alone; without the total too, nothing can be computed
	f := spaceFacts(i64(100*GiB), i64(2048*GiB))
	f.Space.Runs = nil
	if _, code := check(f); code != 1 {
		t.Errorf("no runs, below a tenth: %d", code)
	}
	f.Space.About.Total = nil
	if line, code := check(f); code != 2 || line != "space not known: the storage does not report its total size and no run is recorded yet; no warning can be computed" {
		t.Errorf("no runs, no total: %d %q", code, line)
	}
	// a storage that reports nothing, one that did not answer, a base that is not rclone
	f = spaceFacts(nil, nil)
	f.Space.About = nil
	if line, code := check(f); code != 2 || !strings.Contains(line, "does not report its free space") {
		t.Errorf("nothing: %d %q", code, line)
	}
	f.Space.AboutErr = "rclone said: about is not supported"
	if line, code := check(f); code != 2 || !strings.Contains(line, "did not answer: rclone said: about is not supported") {
		t.Errorf("no answer: %d %q", code, line)
	}
	f.Config.Base = "/srv/repos"
	m := Build(f)
	if line, code := m.CheckLine(); code != 2 || line != "space not known: the storage is not reached through rclone, so it does not report its size; no warning can be computed" {
		t.Errorf("not rclone: %d %q", code, line)
	}
	if l := m.StorageLines(); l[0] != "The backups go to /srv/repos, which is not reached through rclone." {
		t.Errorf("storage lines %q", l)
	}
}

func TestPlainAndRows(t *testing.T) {
	m := Build(spaceFacts(i64(819*GiB), i64(2048*GiB)))
	want := `storage  total 2 TiB  used 1.2 TiB  free 819 GiB
backup  wiki  files  12 GiB  2026-11-14  keep: 8 weekly, 6 monthly, 2 yearly
backup  sites  files  not recorded  keep: 8 weekly, 6 monthly, 2 yearly
backup  proxmox  guests  120 GiB  2026-11-14  keep: shared by its guests
backup  web  guest  32 GiB image  keep: newest 2 images
backup  db  guest  not recorded  keep: newest 5 images
check  space ok: 819 GiB free of 2 TiB, the largest recent run added 31 GiB
note  Do not delete or move files inside the backup folder on the storage by hand; that breaks the backups.`
	if got := strings.Join(m.Plain(), "\n"); got != want {
		t.Errorf("plain:\n%s", got)
	}
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

const layout80 = ` restic-pve v9.9.9 · backup machine · Space                  checked 12 min ago
 ┌────────────────────────────────── Storage ─────────────────────────────────┐
 │ Total 2 TiB · used 1.2 TiB · free 40 GiB                                   │
 │ As the storage drive reports it (rclone about).                            │
 └────────────────────────────────────────────────────────────────────────────┘
 ┌──────────────────── Backups and how long they are kept ────────────────────┐
 │ wiki        files  12 GiB        Sat 14 Nov 8 weekly, 6 monthly, 2 yearly  │
 │ sites       files  not recorded             8 weekly, 6 monthly, 2 yearly  │
 │ proxmox     guests 120 GiB       Sat 14 Nov shared by its guests           │
 │   web       guest  32 GiB image             newest 2 images                │
 │   db        guest  not recorded             newest 5 images                │
 └────────────────────────────────────────────────────────────────────────────┘
 ┌────────────────────────────────── Warning ─────────────────────────────────┐
 │ Warning when free space is below twice the largest recent run (2 x 31 GiB  │
 │ = 62 GiB) or below a tenth of the total (205 GiB).                         │
 │ LOW NOW. Do: lower the keep rules or add storage, see                      │
 │ docs/guide/05-google-drive-space.md.                                       │
 └────────────────────────────────────────────────────────────────────────────┘



 Do not delete or move files inside the backup folder on the storage by hand;
 that breaks the backups.
 Enter all rows  r Refresh  q Back                                 │ restic-pve`

func TestLayout80x24(t *testing.T) {
	old := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = old })
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(80, 24)
	sc := &Screen{Title: "restic-pve v9.9.9 · backup machine · Space", Model: Build(spaceFacts(i64(40*GiB), i64(2048*GiB))),
		Back: "Back", Checked: "checked 12 min ago"}
	sc.Draw(s, retro.NewTheme(s, true))
	if got := strings.Join(text(s), "\n"); got != layout80 {
		t.Errorf("layout:\n%s", got)
	}
}
