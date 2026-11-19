package status

import (
	"strings"
	"testing"
	"time"

	"github.com/norandom/restic-pve/internal/facts"
)

var now = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

func at(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return &t
}

func i64(v int64) *int64 { return &v }

const week = 7 * 24 * time.Hour

// safeFacts is a machine where every copy is recent and checked.
func safeFacts() facts.Facts {
	ok := &facts.Check{TS: *at("2026-11-14T05:50:00Z"), OK: true}
	g := &facts.Check{TS: *at("2026-11-16T03:00:00Z"), OK: true}
	return facts.Facts{
		Version: "v9.9.9",
		Instances: []facts.Instance{
			{Source: "wiki", Name: "wiki", Kind: "files", Newest: at("2026-11-18T05:41:00Z"), NewestChecked: at("2026-11-14T05:41:00Z"), Check: ok, Interval: week, Known: true},
			{Source: "proxmox", Name: "web", Kind: "guest", Newest: at("2026-11-15T01:00:00Z"), NewestChecked: at("2026-11-15T01:00:00Z"), Check: g, Interval: week, Known: true},
		},
		RestoreTest: &facts.RestoreTest{TS: *at("2026-11-15T03:10:00Z"), Status: "success", Unit: "proxmox/web"},
		Next:        []facts.NextRun{{Profile: "local", At: *at("2026-11-21T05:00:00Z")}, {Profile: "guests", At: *at("2026-11-22T00:00:00Z")}},
		Space: facts.SpaceFacts{Remote: "drive", About: &facts.About{Total: i64(2 << 40), Free: i64(819 << 30)},
			Runs: []facts.Run{{DataAdded: 31 << 30}}},
		Alerts: true,
		Proof:  at("2026-11-17T09:00:00Z"),
	}
}

func TestSafe(t *testing.T) {
	m := Build(safeFacts(), "./backup.sh <source>", now)
	want := `copy  wiki  yes  1d  2026-11-18T05:41Z
copy  web  yes  4d  2026-11-15T01:00Z
restore-test  2026-11-15  passed  web
drill  none
running  none
next  local  2026-11-21T05:00Z
next  guests  2026-11-22T00:00Z
space  free 819GiB of 2TiB  ok
updates  v9.9.9  image
alerts  on
password-proof  2026-11-17
verdict  safe`
	if got := strings.Join(m.Plain(), "\n"); got != want {
		t.Errorf("plain:\n%s", got)
	}
	if m.Code() != 0 || m.VerdictLine() != "All copies are recent and checked." {
		t.Errorf("%d %q", m.Code(), m.VerdictLine())
	}
	// alerts off is a valid state and changes no verdict
	f := safeFacts()
	f.Alerts = false
	if m := Build(f, "", now); m.Code() != 0 || !strings.Contains(strings.Join(m.Plain(), "\n"), "alerts  off") {
		t.Errorf("alerts off: %d", m.Code())
	}
}

// TestBoundary: a checked copy exactly interval plus two days old is safe; one minute older is not.
func TestBoundary(t *testing.T) {
	f := safeFacts()
	edge := now.Add(-week - Grace)
	f.Instances[0].NewestChecked = &edge
	if m := Build(f, "./backup.sh <source>", now); m.Code() != 0 {
		t.Errorf("at the edge: %v", m.Plain())
	}
	late := edge.Add(-time.Minute)
	f.Instances[0].NewestChecked = &late
	m := Build(f, "./backup.sh <source>", now)
	if m.Code() != 1 || m.Rows[0].In.Name != "wiki" || m.Rows[0].Reason != "too old" {
		t.Fatalf("late: %v", m.Plain())
	}
	if got := m.Plain()[0]; got != "copy  wiki  no  9d  too old  do: ./backup.sh wiki" {
		t.Errorf("plain %q", got)
	}
	if got := m.VerdictLine(); got != "Not safe: wiki has no checked copy for 9 days. Do: ./backup.sh wiki" {
		t.Errorf("verdict %q", got)
	}
}

func TestNotSafeAndUnknown(t *testing.T) {
	f := safeFacts()
	f.Instances = append(f.Instances,
		facts.Instance{Source: "sites", Name: "sites/blog", Kind: "files", Newest: at("2026-11-14T05:20:00Z"),
			Check: &facts.Check{TS: *at("2026-11-14T05:52:00Z"), OK: false}, Interval: week, Known: true},
		facts.Instance{Source: "proxmox", Name: "db", Kind: "guest", Interval: week, Known: true},
		// no check recorded yet: the newest copy counts, with the note
		facts.Instance{Source: "notes", Name: "notes", Kind: "files", Newest: at("2026-11-13T05:00:00Z"), Interval: week, Known: true},
		facts.Instance{Source: "old", Name: "old", Kind: "files", Newest: at("2026-11-01T05:00:00Z"), Interval: week, Known: true},
	)
	f.RestoreTest = &facts.RestoreTest{TS: *at("2026-11-15T03:10:00Z"), Status: "failed", Unit: "proxmox/web"}
	f.Running = []facts.Job{{Name: "guests", Started: *at("2026-11-19T09:00:00Z"), Running: true}}
	ended := *at("2026-11-14T05:55:10Z")
	three := 3
	f.LastJobs = []facts.Job{{Name: "local", Started: *at("2026-11-14T05:00:00Z"), Ended: &ended, Exit: &three}}
	f.Next[1].At = time.Time{}
	f.Space.About = nil
	f.Proof = nil
	f.Errors = []facts.FactError{{What: "records", Text: "/state/records.json: not readable"}}
	m := Build(f, "./backup.sh <source>", now)
	want := `copy  sites/blog  no  5d  check failed  do: ./backup.sh sites and read the alert
copy  db  no  -  no copy  do: ./backup.sh db
copy  old  no  18d  too old  do: ./backup.sh old
copy  wiki  yes  1d  2026-11-18T05:41Z
copy  web  yes  4d  2026-11-15T01:00Z
copy  notes  yes  6d  2026-11-13T05:00Z  check not recorded yet
restore-test  2026-11-15  failed  web
drill  none
running  guests  since 2026-11-19T09:00Z
last-job  local  ended 2026-11-14T05:55Z  partial
next  local  2026-11-21T05:00Z
next  guests  not-known
space  not-reported
updates  v9.9.9  image
alerts  on
password-proof  none
problem  /state/records.json: not readable
verdict  not-safe`
	if got := strings.Join(m.Plain(), "\n"); got != want {
		t.Errorf("plain:\n%s", got)
	}
	if m.Code() != 1 || m.VerdictLine() != "Not safe: sites/blog failed its last integrity check. Do: ./backup.sh sites and read the alert" {
		t.Errorf("%d %q", m.Code(), m.VerdictLine())
	}

	// storage not reachable: not known, exit 1, the action names the space screen
	f = safeFacts()
	f.Instances[1].Known, f.Instances[1].Problem = false, "rclone said: couldn't connect"
	m = Build(f, "./backup.sh <source>", now)
	if m.Verdict != Unknown || m.Code() != 1 || m.Plain()[0] != "copy  web  unknown  -  storage not reachable  do: ./status.sh space and the sign-in guide docs/guide/05-google-drive-space.md" ||
		m.Plain()[len(m.Plain())-1] != "verdict  unknown" {
		t.Errorf("unknown: %v", m.Plain())
	}
	if !strings.HasPrefix(m.VerdictLine(), "Not known: web cannot be checked: rclone said: couldn't connect. Do: ./status.sh space") {
		t.Errorf("verdict %q", m.VerdictLine())
	}
	f.Instances[1].Problem = facts.WrongPasswordText("rclone:drive:backups/proxmox")
	if r := Build(f, "", now).Rows[0]; r.Reason != "wrong password" || !strings.Contains(r.Do, "password manager") {
		t.Errorf("wrong password row %+v", r)
	}
	// a restore test that failed before a unit was chosen has no unit
	f = safeFacts()
	f.RestoreTest = &facts.RestoreTest{TS: *at("2026-11-15T03:10:00Z"), Status: "failed"}
	if l := Build(f, "", now).Plain(); l[2] != "restore-test  2026-11-15  failed" {
		t.Errorf("no unit: %q", l[2])
	}
	// nothing configured is not safe either
	if m := Build(facts.Facts{}, "", now); m.Code() != 1 || !strings.Contains(m.VerdictLine(), "lists no source") {
		t.Errorf("empty: %d %q", m.Code(), m.VerdictLine())
	}
}

func TestSpaceLine(t *testing.T) {
	s := safeFacts().Space
	if got := SpaceLine(s); got != "819 GiB free of 2 TiB · no warning" {
		t.Errorf("%q", got)
	}
	s.About = &facts.About{Free: i64(40 << 30), Total: i64(2 << 40)}
	if got := SpaceLine(s); !strings.HasPrefix(got, "40 GiB free of 2 TiB · LOW") {
		t.Errorf("%q", got)
	}
	s.About, s.AboutErr = nil, "rclone said: no network"
	if got := SpaceLine(s); got != "not known now: rclone said: no network" {
		t.Errorf("%q", got)
	}
}
