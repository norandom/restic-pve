// Package status is `restic-pve status`: one screen that answers "is there a recent copy
// away from the site?" and "have I seen a restore work?", the plain lines of the same answers
// for scripts, and the exit code (0 only when every source and guest is safe).
package status

import (
	"fmt"
	"strings"
	"time"

	"github.com/norandom/restic-pve/internal/facts"
)

// Verdict is the answer for one instance or for all of them.
type Verdict int

const (
	Safe Verdict = iota
	NotSafe
	Unknown
)

// Grace is what a copy may be late beyond its schedule interval before it is not safe.
const Grace = 48 * time.Hour

// Guides named on the screens.
const (
	DrillGuide  = "docs/guide/07-restore.md"
	BadDayGuide = "docs/guide/08-when-something-goes-wrong.md"
	SpaceGuide  = "docs/guide/05-google-drive-space.md"
	SpaceCmd    = "./status.sh space"
)

// Row is the answer for one local source instance or guest.
type Row struct {
	In      facts.Instance
	Verdict Verdict
	Age     time.Duration // of the copy the row talks about; -1: no copy
	At      *time.Time    // that copy
	Reason  string        // why not safe or not known: "no copy", "check failed", ...
	Why     string        // the reason as part of a sentence: "has no copy yet"
	Do      string        // the one action
	Note    string        // "check not recorded yet"
}

// Model is the whole status, from the facts.
type Model struct {
	F       facts.Facts
	Rows    []Row // not safe first, then not known, then safe
	Verdict Verdict
	Now     time.Time
	Hint    string     // the start command (RESTIC_PVE_START_HINT)
	Reads   []ReadsRow // guests that read only changed blocks, those with a fault first
}

// Build applies the safety rule: an instance is safe when its newest checked copy is not
// older than its interval plus two days. Until the records hold a check of its repository,
// the newest copy counts, with the note "check not recorded yet".
func Build(f facts.Facts, hint string, now time.Time) Model {
	m := Model{F: f, Now: now, Hint: hint, Reads: readsRows(f, hint)}
	var groups [3][]Row
	for _, in := range f.Instances {
		r := row(in, hint, now)
		groups[r.Verdict] = append(groups[r.Verdict], r)
	}
	m.Rows = append(append(groups[NotSafe], groups[Unknown]...), groups[Safe]...)
	switch {
	case len(groups[NotSafe]) > 0:
		m.Verdict = NotSafe
	case len(groups[Unknown]) > 0 || len(f.Instances) == 0:
		m.Verdict = Unknown
	}
	return m
}

func row(in facts.Instance, hint string, now time.Time) Row {
	r := Row{In: in, Age: -1}
	start := facts.Hint(hint, in.Source)
	if in.Kind == "guest" {
		start = facts.Hint(hint, in.Name)
	}
	if in.Newest != nil {
		r.At, r.Age = in.Newest, now.Sub(*in.Newest)
	}
	switch {
	case !in.Known:
		r.Verdict, r.Reason, r.Why = Unknown, "storage not reachable", "cannot be checked: "+in.Problem
		r.Do = SpaceCmd + " and the sign-in guide " + SpaceGuide
		if in.Problem == facts.NotRead {
			r.Reason, r.Why, r.Do = "not read yet", "is not read from the storage yet", "wait a moment, or press r"
		} else if strings.Contains(in.Problem, "never make a new password") {
			r.Reason, r.Why = "wrong password", "cannot be opened with the password in use"
			r.Do = "put the copy from your password manager into .env as RESTIC_PASSWORD=..."
		} else if strings.Contains(in.Problem, "RESTIC_PASSWORD is not set") {
			r.Reason, r.Why, r.Do = "password not set", "cannot be read: RESTIC_PASSWORD is not set", "set RESTIC_PASSWORD in .env"
		}
	case in.Newest == nil:
		r.Verdict, r.Reason, r.Why, r.Do = NotSafe, "no copy", "has no copy yet", start
	case in.Check != nil && !in.Check.OK:
		r.Verdict, r.Reason, r.Why, r.Do = NotSafe, "check failed", "failed its last integrity check", start+" and read the alert"
	case in.Check == nil:
		r.Note = "check not recorded yet"
		if r.Age > in.Interval+Grace {
			r.Verdict, r.Reason = NotSafe, "too old"
			r.Why, r.Do = fmt.Sprintf("has no copy for %s", days(r.Age)), start
		}
	case in.NewestChecked == nil:
		r.Verdict, r.Reason, r.Why, r.Do = NotSafe, "not checked yet", "has no checked copy yet", start
	default:
		age := now.Sub(*in.NewestChecked)
		if age > in.Interval+Grace {
			r.Verdict, r.Reason, r.At, r.Age = NotSafe, "too old", in.NewestChecked, age
			r.Why, r.Do = fmt.Sprintf("has no checked copy for %s", days(age)), start
		}
	}
	return r
}

// Code is the exit code of the plain form: 0 only when every instance is safe.
func (m Model) Code() int {
	if m.Verdict == Safe {
		return 0
	}
	return 1
}

// days is an age in words: "today", "1 day", "12 days".
func days(d time.Duration) string {
	n := int(d / (24 * time.Hour))
	switch {
	case d < 0:
		return "-"
	case n == 0:
		return "today"
	case n == 1:
		return "1 day"
	}
	return fmt.Sprintf("%d days", n)
}

func short(d time.Duration) string {
	if d < 0 {
		return "-"
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

func utc(t time.Time) string { return t.UTC().Format("2006-01-02T15:04Z") }

// VerdictLine is the line under the boxes: all safe, or the first instance that is not, with
// its reason and its one action.
func (m Model) VerdictLine() string {
	switch {
	case m.Verdict == Safe:
		return "All copies are recent and checked."
	case len(m.Rows) == 0:
		return "Not known: backup.toml lists no source and no guest. Do: add one to backup.toml"
	}
	r := m.Rows[0]
	word := "Not safe"
	if r.Verdict == Unknown {
		word = "Not known"
	}
	return fmt.Sprintf("%s: %s %s. Do: %s", word, r.In.Name, r.Why, r.Do)
}

// restoreWords are the words of a restore test result.
var restoreWords = map[string]string{"success": "passed", "partial": "partly passed", "failed": "FAILED"}

func unitName(unit string) string {
	if _, after, ok := strings.Cut(unit, "/"); ok {
		return after
	}
	return unit
}

func exitWord(code *int) string {
	switch {
	case code == nil:
		return "no result"
	case *code == 0:
		return "ok"
	case *code == 3:
		return "partial"
	}
	return fmt.Sprintf("failed (exit %d)", *code)
}

// SpaceLine is the one line about the storage space, for the screen.
func SpaceLine(s facts.SpaceFacts) string {
	if s.About == nil || s.About.Free == nil {
		if s.AboutErr != "" {
			return "not known now: " + s.AboutErr
		}
		return "the storage does not report its free space"
	}
	free := facts.Size(*s.About.Free) + " free"
	if s.About.Total != nil {
		free += " of " + facts.Size(*s.About.Total)
	}
	switch s.Check() {
	case facts.SpaceLow:
		return free + " · LOW: lower the keep rules or add storage (s Space)"
	case facts.SpaceOK:
		return free + " · no warning"
	}
	return free + " · no warning can be computed yet"
}

func spacePlain(s facts.SpaceFacts) string {
	if s.About == nil || s.About.Free == nil {
		return "space  not-reported"
	}
	nb := func(b int64) string { return strings.ReplaceAll(facts.Size(b), " ", "") }
	line := "space  free " + nb(*s.About.Free)
	if s.About.Total != nil {
		line += " of " + nb(*s.About.Total)
	}
	return line + "  " + map[facts.SpaceState]string{facts.SpaceOK: "ok", facts.SpaceLow: "low", facts.SpaceUnknown: "unknown"}[s.Check()]
}

// Plain is the status as plain lines: one fact per line, words separated by two spaces.
func (m Model) Plain() []string {
	var out []string
	add := func(parts ...string) { out = append(out, strings.Join(parts, "  ")) }
	for _, r := range m.Rows {
		switch {
		case r.Verdict == Safe:
			p := []string{"copy", r.In.Name, "yes", short(r.Age), utc(*r.At)}
			if r.Note != "" {
				p = append(p, r.Note)
			}
			add(p...)
		case r.Verdict == NotSafe:
			add("copy", r.In.Name, "no", short(r.Age), r.Reason, "do: "+r.Do)
		default:
			add("copy", r.In.Name, "unknown", "-", r.Reason, "do: "+r.Do)
		}
	}
	for _, r := range m.Reads {
		out = append(out, r.plain...)
	}
	f := m.F
	if t := f.RestoreTest; t != nil {
		p := []string{"restore-test", t.TS.UTC().Format("2006-01-02"), map[string]string{"success": "passed", "partial": "partial",
			"failed": "failed"}[t.Status]}
		if t.Unit != "" { // null when the test failed before a unit was chosen
			p = append(p, unitName(t.Unit))
		}
		add(p...)
	} else {
		add("restore-test", "none")
	}
	if f.Drill != nil {
		add("drill", f.Drill.UTC().Format("2006-01-02"))
	} else {
		add("drill", "none")
	}
	if len(f.Running) == 0 {
		add("running", "none")
	}
	for _, j := range f.Running {
		if j.Started.IsZero() {
			add("running", j.Name)
		} else {
			add("running", j.Name, "since "+utc(j.Started))
		}
	}
	if len(f.LastJobs) > 0 {
		j := f.LastJobs[0]
		if j.Ended != nil {
			add("last-job", j.Name, "ended "+utc(*j.Ended), exitWord(j.Exit))
		} else {
			add("last-job", j.Name, "started "+utc(j.Started), "ended without a result")
		}
	}
	for _, n := range f.Next {
		if n.At.IsZero() {
			add("next", n.Profile, "not-known")
		} else {
			add("next", n.Profile, utc(n.At))
		}
	}
	add(spacePlain(f.Space))
	add("updates", f.Version, "image")
	add("alerts", onOff(f.Alerts))
	if f.Proof != nil {
		add("password-proof", f.Proof.UTC().Format("2006-01-02"))
	} else {
		add("password-proof", "none")
	}
	for _, e := range f.Errors {
		add("problem", e.Text)
	}
	add("verdict", map[Verdict]string{Safe: "safe", NotSafe: "not-safe", Unknown: "unknown"}[m.Verdict])
	return out
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
