package space

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/norandom/restic-pve/internal/facts"
	"github.com/norandom/restic-pve/retro"
)

func workFacts() facts.Facts {
	f := spaceFacts(i64(40*GiB), i64(2048*GiB))
	f.Space.Work = &facts.WorkSpace{Dir: "/state/pieces", Total: 1*GiB + 24<<10,
		Guests: []facts.GuestWork{{Alias: "web", Bytes: 1 * GiB}, {Alias: "db", Bytes: 16 << 10}}, Free: i64(812 * GiB)}
	return f
}

func TestWorkingSpace(t *testing.T) {
	old := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = old })
	m := Build(workFacts())
	const row = "Pieces working space: 1 GiB in total (web 1 GiB, db 16 KiB) in /state/pieces, 812 GiB free"
	if got := m.WorkLine(); got != row {
		t.Errorf("work line %q", got)
	}
	if p := strings.Join(m.Plain(), "\n"); !strings.Contains(p, "\nwork  /state/pieces  total 1GiB  web 1GiB  db 16KiB  free 812GiB\n") {
		t.Errorf("plain:\n%s", p)
	}
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(80, 24)
	sc := &Screen{Title: "restic-pve v9.9.9 · backup machine · Space", Model: m, Back: "Back", Checked: "checked 12 min ago"}
	sc.Draw(s, retro.NewTheme(s, true))
	l := text(s)
	want := []string{
		" │ Total 2 TiB · used 1.2 TiB · free 40 GiB                                   │",
		" │ As the storage drive reports it (rclone about).                            │",
		" │ Pieces working space: 1 GiB in total (web 1 GiB, db 16 KiB) in             │",
		" │ /state/pieces, 812 GiB free                                                │",
		" └────────────────────────────────────────────────────────────────────────────┘",
		" ┌──────────────────── Backups and how long they are kept ────────────────────┐",
	}
	if strings.Join(l[2:8], "\n") != strings.Join(want, "\n") {
		t.Errorf("layout:\n%s", strings.Join(l, "\n"))
	}
	// the rows below still fit: the last guest row and the fixed sentence
	if all := strings.Join(l, "\n"); !strings.Contains(all, "│   db        guest  not recorded") || !strings.Contains(all, " that breaks the backups.") {
		t.Errorf("layout:\n%s", all)
	}
	// without pieces runs there is no row
	if Build(spaceFacts(i64(40*GiB), nil)).WorkLine() != "" {
		t.Error("a work line without working space")
	}
}
