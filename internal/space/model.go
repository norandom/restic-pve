// Package space is `restic-pve space`: how full the storage is, how big the backups are and
// how long they are kept, and the warning rule that tells the admin before the storage fills.
// `space --check` prints the rule's result as one line for backupctl.
package space

import (
	"fmt"
	"strings"
	"time"

	"github.com/norandom/restic-pve/internal/facts"
)

// The fixed texts.
const (
	DontTouch = "Do not delete or move files inside the backup folder on the storage by hand; that breaks the backups."
	Guide     = "docs/guide/05-google-drive-space.md"
	Action    = "lower the keep rules or add storage, see " + Guide
)

// Row is one source, Proxmox host or guest.
type Row struct {
	Name, Kind string
	Size       string // "" when not recorded
	When       time.Time
	Keep       string
}

// Model is the space view.
type Model struct {
	F     facts.SpaceFacts
	Rows  []Row
	State facts.SpaceState
	Base  string
}

// Build makes the view from the facts.
func Build(f facts.Facts) Model {
	m := Model{F: f.Space, State: f.Space.Check(), Base: f.Config.Base}
	c := f.Config
	size := func(source string) (string, time.Time) {
		if s, ok := f.Space.RepoSizes[source]; ok {
			return facts.Size(s.Bytes), s.TS
		}
		return "", time.Time{}
	}
	for _, s := range c.Files {
		sz, when := size(s)
		m.Rows = append(m.Rows, Row{Name: s, Kind: "files", Size: sz, When: when, Keep: c.Retention.KeepWords()})
	}
	for _, p := range c.Proxmox {
		sz, when := size(p.Name)
		m.Rows = append(m.Rows, Row{Name: p.Name, Kind: "guests", Size: sz, When: when, Keep: "shared by its guests"})
		for _, g := range p.Guests {
			r := Row{Name: "  " + g.Alias, Kind: "guest", Keep: fmt.Sprintf("newest %d images", g.KeepLast)}
			if b := f.Space.GuestSize[g.Alias]; b != nil {
				r.Size = facts.Size(*b) + " image"
			}
			m.Rows = append(m.Rows, r)
		}
	}
	return m
}

// Unknown says why no warning can be computed.
func (m Model) Unknown() string {
	a := m.F.About
	switch {
	case facts.Remote(m.Base) == "":
		return "the storage is not reached through rclone, so it does not report its size"
	case m.F.AboutErr != "":
		return "the storage did not answer: " + m.F.AboutErr
	case a == nil || a.Free == nil:
		return "the storage does not report its free space"
	}
	return "the storage does not report its total size and no run is recorded yet"
}

// CheckLine is the one line of `space --check` and its exit code: 1 on a warning, 0 when
// there is none, 2 when the numbers are unknown.
func (m Model) CheckLine() (string, int) {
	switch m.State {
	case facts.SpaceUnknown:
		return "space not known: " + m.Unknown() + "; no warning can be computed", 2
	case facts.SpaceLow:
		free := *m.F.About.Free
		d, runs := m.F.Largest()
		if runs && free < 2*d {
			return fmt.Sprintf("space low: %s free, the largest recent run added %s; %s", facts.Size(free), facts.Size(d), Action), 1
		}
		return fmt.Sprintf("space low: %s free of %s, less than a tenth; %s", facts.Size(free), facts.Size(*m.F.About.Total), Action), 1
	}
	line := "space ok: " + facts.Size(*m.F.About.Free) + " free"
	if t := m.F.About.Total; t != nil {
		line += " of " + facts.Size(*t)
	}
	if d, runs := m.F.Largest(); runs {
		line += ", the largest recent run added " + facts.Size(d)
	}
	return line, 0
}

func opt(p *int64) string {
	if p == nil {
		return "not reported"
	}
	return facts.Size(*p)
}

// StorageLines are the lines of the Storage box.
func (m Model) StorageLines() []string {
	a := m.F.About
	remote := facts.Remote(m.Base)
	switch {
	case remote == "":
		return []string{"The backups go to " + m.Base + ", which is not reached through rclone.",
			"It does not report its size. No warning can be computed."}
	case a == nil && m.F.AboutErr != "":
		return []string{"Storage " + remote + ": not reachable now.", m.F.AboutErr}
	case a == nil:
		return []string{"Storage " + remote + ": the storage does not report its size.", "No warning can be computed."}
	}
	l := []string{fmt.Sprintf("Total %s · used %s · free %s", opt(a.Total), opt(a.Used), opt(a.Free)),
		"As the storage " + remote + " reports it (rclone about)."}
	if a.Trashed != nil && *a.Trashed > 0 {
		l[0] += " · in the trash " + facts.Size(*a.Trashed)
	}
	switch {
	case a.Free == nil:
		l[1] = "The storage does not report its free space. No warning can be computed."
	case a.Total == nil:
		l[1] = "The storage does not report its total size."
	}
	return l
}

// WarningLines are the rule in words with today's numbers, and what to do.
func (m Model) WarningLines() []string {
	d, runs := m.F.Largest()
	rule := "Warning when free space is below twice the largest recent run"
	if runs {
		rule += fmt.Sprintf(" (2 x %s = %s)", facts.Size(d), facts.Size(2*d))
	} else {
		rule += " (no run recorded yet)"
	}
	rule += " or below a tenth of the total"
	if a := m.F.About; a != nil && a.Total != nil {
		rule += fmt.Sprintf(" (%s)", facts.Size(*a.Total/10))
	}
	rule += "."
	line, _ := m.CheckLine()
	switch m.State {
	case facts.SpaceLow:
		return []string{rule, "LOW NOW. Do: " + Action + "."}
	case facts.SpaceOK:
		return []string{rule, "Now: " + strings.TrimPrefix(line, "space ok: ") + ". No warning."}
	}
	return []string{rule, "Now: " + m.Unknown() + "; no warning can be computed."}
}

// Plain is the space view as plain lines.
func (m Model) Plain() []string {
	var out []string
	a := m.F.About
	if a != nil {
		out = append(out, "storage  total "+opt(a.Total)+"  used "+opt(a.Used)+"  free "+opt(a.Free))
	} else {
		out = append(out, "storage  "+m.Unknown())
	}
	for _, r := range m.Rows {
		size := r.Size
		if size == "" {
			size = "not recorded"
		}
		when := ""
		if !r.When.IsZero() {
			when = "  " + r.When.UTC().Format("2006-01-02")
		}
		out = append(out, "backup  "+strings.TrimSpace(r.Name)+"  "+r.Kind+"  "+size+when+"  keep: "+r.Keep)
	}
	line, _ := m.CheckLine()
	return append(out, "check  "+line, "note  "+DontTouch)
}
